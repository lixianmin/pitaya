// Copyright (c) TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package client

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/topfreegames/pitaya/acceptor"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"github.com/topfreegames/pitaya"
	"github.com/topfreegames/pitaya/conn/codec"
	"github.com/topfreegames/pitaya/conn/message"
	"github.com/topfreegames/pitaya/conn/packet"
	"github.com/topfreegames/pitaya/logger"
	"github.com/topfreegames/pitaya/session"
	"github.com/topfreegames/pitaya/util/compression"
)

// HandshakeSys struct
type HandshakeSys struct {
	Dict       map[string]uint16 `json:"dict"`
	Heartbeat  int               `json:"heartbeat"`
	Serializer string            `json:"serializer"`
}

// HandshakeData struct
type HandshakeData struct {
	Code int          `json:"code"`
	Sys  HandshakeSys `json:"sys"`
}

type pendingRequest struct {
	msg    *message.Message
	sentAt time.Time
}

// Client struct
type Client struct {
	conn                net.Conn
	Connected           bool
	packetEncoder       codec.PacketEncoder
	packetDecoder       codec.PacketDecoder
	packetChan          chan *packet.Packet      // 所有从server端收到的packet，都先放到packetChan中
	IncomingMsgChan     chan *message.Message    // 这里的msg分两部分：1 正常收到server端的response的message；2 超时的message
	pendingChan         chan bool                // 控制发送速度
	pendingRequests     map[uint]*pendingRequest // 发请求到sever，并等待处理结果的request
	pendingReqMutex     sync.Mutex               // 监控pendingRequests
	requestTimeout      time.Duration            // 默认5s，抛弃超时的请求
	closeChan           chan struct{}            // 这个closeChan，原理跟我的WaitDispose一模一样，用于控制对象的协程退出
	nextID              uint32                   // 用于生成Message的ID，因此每个client使用一个各自的ID序列
	messageEncoder      message.Encoder
	clientHandshakeData *session.HandshakeData
}

// MsgChannel return the incoming message channel
func (c *Client) MsgChannel() chan *message.Message {
	return c.IncomingMsgChan
}

// ConnectedStatus return the connection status
func (c *Client) ConnectedStatus() bool {
	return c.Connected
}

// New returns a new client
// piata里经常使用『可变参数』实现默认参数，这种方法的优点是调用时可以不填，缺点是很难扩展第二个可选参数
func New(logLevel logrus.Level, requestTimeout ...time.Duration) *Client {
	l := logrus.New()
	l.Formatter = &logrus.TextFormatter{}
	l.SetLevel(logLevel)

	logger.Log = l

	reqTimeout := 5 * time.Second
	if len(requestTimeout) > 0 {
		reqTimeout = requestTimeout[0]
	}

	return &Client{
		Connected:       false,
		packetEncoder:   codec.NewPomeloPacketEncoder(),
		packetDecoder:   codec.NewPomeloPacketDecoder(),
		packetChan:      make(chan *packet.Packet, 10),
		pendingRequests: make(map[uint]*pendingRequest),
		requestTimeout:  reqTimeout,
		// 30 here is the limit of inflight（飞行中的） messages
		// TODO this should probably be configurable
		pendingChan:    make(chan bool, 30),
		messageEncoder: message.NewMessagesEncoder(false),
		clientHandshakeData: &session.HandshakeData{
			Sys: session.HandshakeClientData{
				Platform:    "mac",
				LibVersion:  "0.3.5-release",
				BuildNumber: "20",
				Version:     "2.1",
			},
			User: map[string]interface{}{
				"age": 30,
			},
		},
	}
}

// SetClientHandshakeData sets the data to send inside handshake
func (c *Client) SetClientHandshakeData(data *session.HandshakeData) {
	c.clientHandshakeData = data
}

func (c *Client) sendHandshakeRequest() error {
	enc, err := json.Marshal(c.clientHandshakeData)
	if err != nil {
		return err
	}

	p, err := c.packetEncoder.Encode(packet.Handshake, enc)
	if err != nil {
		return err
	}

	_, err = c.conn.Write(p)
	return err
}

func (c *Client) handleHandshakeResponse() error {
	buf := bytes.NewBuffer(nil)
	packets, err := c.readPackets(buf)
	if err != nil {
		return err
	}

	handshakePacket := packets[0]
	if handshakePacket.Type != packet.Handshake {
		return fmt.Errorf("got first packet from server that is not a handshake, aborting")
	}

	handshake := &HandshakeData{}
	if compression.IsCompressed(handshakePacket.Data) {
		handshakePacket.Data, err = compression.InflateData(handshakePacket.Data)
		if err != nil {
			return err
		}
	}

	err = json.Unmarshal(handshakePacket.Data, handshake)
	if err != nil {
		return err
	}

	logger.Log.Debug("got handshake from sv, data: %v", handshake)

	if handshake.Sys.Dict != nil {
		message.SetDictionary(handshake.Sys.Dict)
	}
	p, err := c.packetEncoder.Encode(packet.HandshakeAck, []byte{})
	if err != nil {
		return err
	}
	_, err = c.conn.Write(p)
	if err != nil {
		return err
	}

	c.Connected = true

	go c.sendHeartbeats(handshake.Sys.Heartbeat)
	go c.handleServerMessages()
	go c.handlePackets()
	go c.pendingRequestsReaper()

	return nil
}

// pendingRequestsReaper delete timedout requests
// reaper：收割机
func (c *Client) pendingRequestsReaper() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			toDelete := make([]*pendingRequest, 0)
			c.pendingReqMutex.Lock()
			for _, v := range c.pendingRequests {
				if time.Now().Sub(v.sentAt) > c.requestTimeout {
					toDelete = append(toDelete, v)
				}
			}
			for _, pendingReq := range toDelete {
				err := pitaya.Error(errors.New("request timeout"), "PIT-504")
				errMarshalled, _ := json.Marshal(err)
				// send a timeout to incoming msg chan
				m := &message.Message{
					Type:  message.Response,
					ID:    pendingReq.msg.ID,
					Route: pendingReq.msg.Route,
					Data:  errMarshalled,
					Err:   true,
				}
				delete(c.pendingRequests, pendingReq.msg.ID)
				<-c.pendingChan
				// 每秒检测超时的请求，将它们从pendingRequests中移除，伪装成Message放到IncomingMsgChan中
				c.IncomingMsgChan <- m
			}
			c.pendingReqMutex.Unlock()
		case <-c.closeChan:
			return
		}
	}
}

// 处理从网络上收到的packet包
func (c *Client) handlePackets() {
	for {
		select {
		case p := <-c.packetChan:
			switch p.Type {
			case packet.Data:
				//handle data
				logger.Log.Debug("got data: %s", string(p.Data))
				m, err := message.Decode(p.Data)
				if err != nil {
					logger.Log.Errorf("error decoding msg from sv: %s", string(m.Data))
				}
				if m.Type == message.Response {
					c.pendingReqMutex.Lock()
					if _, ok := c.pendingRequests[m.ID]; ok {
						delete(c.pendingRequests, m.ID)
						<-c.pendingChan
					} else {
						c.pendingReqMutex.Unlock()
						continue // do not process msg for already timedout request
					}
					c.pendingReqMutex.Unlock()
				}
				// 已经超时的response直接扔掉，有效的response放到IncomingMsgChan中
				c.IncomingMsgChan <- m
			case packet.Kick:
				logger.Log.Warn("got kick packet from the server! disconnecting...")
				c.Disconnect()
			}
		case <-c.closeChan:
			return
		}
	}
}

func (c *Client) readPackets(buf *bytes.Buffer) ([]*packet.Packet, error) {
	// listen for sv messages
	data := make([]byte, 1024)
	n := len(data)
	var err error

	for n == len(data) {
		n, err = c.conn.Read(data)
		if err != nil {
			return nil, err
		}
		buf.Write(data[:n])
	}
	packets, err := c.packetDecoder.Decode(buf.Bytes())
	if err != nil {
		logger.Log.Errorf("error decoding packet from server: %s", err.Error())
	}
	totalProcessed := 0
	for _, p := range packets {
		totalProcessed += codec.HeadLength + p.Length
	}
	buf.Next(totalProcessed)

	return packets, nil
}

// 将接收到的网络包通过c.packetChan中转到另一个协程中，讲道理也没有搞明白是为了什么？
// 个人认为网络收发各自使用一个协程已经够奢侈了
func (c *Client) handleServerMessages() {
	// bytes.NewBuffer()这个东西，很像是其它语言中的StringBuilder
	buf := bytes.NewBuffer(nil)
	defer c.Disconnect()
	//这个协程通过c.Connected，而不是closeChan来控制退出
	for c.Connected {
		packets, err := c.readPackets(buf)
		if err != nil && c.Connected {
			logger.Log.Error(err)
			break
		}

		for _, p := range packets {
			c.packetChan <- p
		}
	}
}

// sendHeartBeats()设计为一个单独的协程
// 优点是：
// 1. 延迟有可能会低。但是大家都是通过c.conn.Write()写出，因此这个还真不好说
// 2. 代码逻辑相对集中
//
// 缺点是：
// 1. 多了一个协程，占用更多的系统资源
func (c *Client) sendHeartbeats(interval int) {
	t := time.NewTicker(time.Duration(interval) * time.Second)
	defer func() {
		t.Stop()
		c.Disconnect()
	}()
	for {
		select {
		case <-t.C:
			p, _ := c.packetEncoder.Encode(packet.Heartbeat, []byte{})
			_, err := c.conn.Write(p)
			if err != nil {
				logger.Log.Errorf("error sending heartbeat to server: %s", err.Error())
				return
			}
		case <-c.closeChan:
			return
		}
	}
}

// Disconnect disconnects the client
func (c *Client) Disconnect() {
	if c.Connected {
		c.Connected = false
		close(c.closeChan)
		c.conn.Close()
	}
}

// ConnectTo connects to the server at addr, for now the only supported protocol is tcp
// if tlsConfig is sent, it connects using TLS
func (c *Client) ConnectTo(addr string, tlsConfig ...*tls.Config) error {
	var conn net.Conn
	var err error
	if len(tlsConfig) > 0 {
		conn, err = tls.Dial("tcp", addr, tlsConfig[0])
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	c.conn = conn
	c.IncomingMsgChan = make(chan *message.Message, 10)

	if err = c.handleHandshake(); err != nil {
		return err
	}

	c.closeChan = make(chan struct{})

	return nil
}

// ConnectToWS connects using webshocket protocol
func (c *Client) ConnectToWS(addr string, path string, tlsConfig ...*tls.Config) error {
	u := url.URL{Scheme: "ws", Host: addr, Path: path}
	dialer := websocket.DefaultDialer

	if len(tlsConfig) > 0 {
		dialer.TLSClientConfig = tlsConfig[0]
		u.Scheme = "wss"
	}

	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}

	c.conn, err = acceptor.NewWSConn(conn)
	if err != nil {
		return err
	}

	c.IncomingMsgChan = make(chan *message.Message, 10)

	if err = c.handleHandshake(); err != nil {
		return err
	}

	c.closeChan = make(chan struct{})

	return nil
}

func (c *Client) handleHandshake() error {
	if err := c.sendHandshakeRequest(); err != nil {
		return err
	}

	if err := c.handleHandshakeResponse(); err != nil {
		return err
	}
	return nil
}

// SendRequest sends a request to the server
func (c *Client) SendRequest(route string, data []byte) (uint, error) {
	return c.sendMsg(message.Request, route, data)
}

// SendNotify sends a notify to the server
func (c *Client) SendNotify(route string, data []byte) error {
	_, err := c.sendMsg(message.Notify, route, data)
	return err
}

func (c *Client) buildPacket(msg message.Message) ([]byte, error) {
	encMsg, err := c.messageEncoder.Encode(&msg)
	if err != nil {
		return nil, err
	}
	p, err := c.packetEncoder.Encode(packet.Data, encMsg)
	if err != nil {
		return nil, err
	}

	return p, nil
}

// sendMsg sends the request to the server
func (c *Client) sendMsg(msgType message.Type, route string, data []byte) (uint, error) {
	// TODO mount msg and encode
	m := message.Message{
		Type: msgType,
		//每一个消息都自带一个消息ID
		ID:    uint(atomic.AddUint32(&c.nextID, 1)),
		Route: route,
		Data:  data,
		Err:   false,
	}
	p, err := c.buildPacket(m)
	if msgType == message.Request {
		c.pendingChan <- true
		c.pendingReqMutex.Lock()
		//如果是异步处理的话，通过这个消息可以找到原始的消息，这听起来非常像秒杀活动中异步处理http请求的方案
		if _, ok := c.pendingRequests[m.ID]; !ok {
			c.pendingRequests[m.ID] = &pendingRequest{
				msg:    &m,
				sentAt: time.Now(),
			}
		}
		c.pendingReqMutex.Unlock()
	}

	if err != nil {
		return m.ID, err
	}
	_, err = c.conn.Write(p)
	return m.ID, err
}
