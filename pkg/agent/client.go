/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"io"
	"net"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
)

// connContext tracks a connection from agent to node network.
type connContext struct {
	conn      net.Conn
	cleanFunc func()
	dataCh    chan []byte
	cleanOnce sync.Once
}

func (c *connContext) cleanup() {
	c.cleanOnce.Do(c.cleanFunc)
}

type connectionManager struct {
	mu          sync.RWMutex
	connections map[int64]*connContext
}

func (cm *connectionManager) Add(connID int64, ctx *connContext) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.connections[connID] = ctx
}

func (cm *connectionManager) Get(connID int64) (*connContext, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	ctx, ok := cm.connections[connID]
	return ctx, ok
}

func (cm *connectionManager) Delete(connID int64) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	delete(cm.connections, connID)
}

func newConnectionManager() *connectionManager {
	return &connectionManager{
		connections: make(map[int64]*connContext),
	}
}

// AgentClient runs on the node network side. It connects to proxy server and establishes
// a stream connection from which it sends and receives network traffic.
type AgentClient struct {
	nextConnID int64

	connManager *connectionManager

	stream *RedialableAgentClient
	stopCh <-chan struct{}
}

func newAgentClient(address, agentID string, cs *ClientSet, opts ...grpc.DialOption) (*AgentClient, error) {
	stream, err := NewRedialableAgentClient(address, agentID, cs, opts...)
	if err != nil {
		return nil, err
	}
	return newAgentClientWithRedialableAgentClient(stream), nil
}

func newAgentClientWithRedialableAgentClient(rac *RedialableAgentClient) *AgentClient {
	return &AgentClient{
		connManager: newConnectionManager(),
		stream:      rac,
		stopCh:      rac.stopCh,
	}
}

// Close closes the underlying stream.
func (c *AgentClient) Close() {
	if c.stream == nil {
		klog.Warning("Unexpected empty AgentClient.stream")
		return
	}
	c.stream.Close()
}

// Connect connects to proxy server to establish a gRPC stream,
// on which the proxied traffic is multiplexed through the stream
// and piped to the local connection. It register itself as a
// backend from proxy server, so proxy server will route traffic
// to this agent.
//
// The caller needs to call Serve to start serving proxy requests
// coming from proxy server.

// Serve starts to serve proxied requests from proxy server over the
// gRPC stream. Successful Connect is required before Serve. The
// The requests include things like opening a connection to a server,
// streaming data and close the connection.
func (a *AgentClient) Serve() {
	klog.Infof("Start serving for serverID %s", a.stream.serverID)
	go a.stream.probe()
	for {
		select {
		case <-a.stopCh:
			klog.Info("stop agent client.")
			return
		default:
		}

		pkt, err := a.stream.Recv()
		if err != nil {
			if err2, ok := err.(*ReconnectError); ok {
				err3 := err2.Wait()
				if err3 != nil {
					klog.Warningf("reconnect error: %v", err3)
				}
				continue
			} else if err == io.EOF {
				klog.Info("received EOF, exit")
				return
			}
		}

		if err != nil {
			klog.Warningf("stream read error: %v", err)
			return
		}

		klog.Infof("[tracing] recv packet, type: %s", pkt.Type)

		if pkt == nil {
			klog.Warningf("empty packet received")
			continue
		}

		switch pkt.Type {
		case client.PacketType_DIAL_REQ:
			klog.Info("received DIAL_REQ")
			resp := &client.Packet{
				Type:    client.PacketType_DIAL_RSP,
				Payload: &client.Packet_DialResponse{DialResponse: &client.DialResponse{}},
			}

			dialReq := pkt.GetDialRequest()
			resp.GetDialResponse().Random = dialReq.Random

			conn, err := net.Dial(dialReq.Protocol, dialReq.Address)
			if err != nil {
				resp.GetDialResponse().Error = err.Error()
				if err := a.stream.RetrySend(resp); err != nil {
					klog.Warningf("stream send error: %v", err)
				}
				continue
			}

			connID := atomic.AddInt64(&a.nextConnID, 1)
			dataCh := make(chan []byte, 5)
			ctx := &connContext{
				conn:   conn,
				dataCh: dataCh,
				cleanFunc: func() {
					klog.Infof("close connection(id=%d)", connID)
					resp := &client.Packet{
						Type:    client.PacketType_CLOSE_RSP,
						Payload: &client.Packet_CloseResponse{CloseResponse: &client.CloseResponse{}},
					}
					resp.GetCloseResponse().ConnectID = connID

					err := conn.Close()
					if err != nil {
						resp.GetCloseResponse().Error = err.Error()
					}

					if err := a.stream.RetrySend(resp); err != nil {
						klog.Warningf("close response send error: %v", err)
					}

					close(dataCh)
					a.connManager.Delete(connID)
				},
			}
			a.connManager.Add(connID, ctx)

			resp.GetDialResponse().ConnectID = connID
			if err := a.stream.RetrySend(resp); err != nil {
				klog.Warningf("stream send error: %v", err)
				continue
			}

			go a.remoteToProxy(connID, ctx)
			go a.proxyToRemote(connID, ctx)

		case client.PacketType_DATA:
			data := pkt.GetData()
			klog.Infof("received DATA(id=%d)", data.ConnectID)

			ctx, ok := a.connManager.Get(data.ConnectID)
			if ok {
				ctx.dataCh <- data.Data
			}

		case client.PacketType_CLOSE_REQ:
			closeReq := pkt.GetCloseRequest()
			connID := closeReq.ConnectID

			klog.Infof("received CLOSE_REQ(id=%d)", connID)

			ctx, ok := a.connManager.Get(connID)
			if ok {
				ctx.cleanup()
			} else {
				resp := &client.Packet{
					Type:    client.PacketType_CLOSE_RSP,
					Payload: &client.Packet_CloseResponse{CloseResponse: &client.CloseResponse{}},
				}
				resp.GetCloseResponse().ConnectID = connID
				resp.GetCloseResponse().Error = "Unknown connectID"
				if err := a.stream.Send(resp); err != nil {
					klog.Warningf("close response send error: %v", err)
					continue
				}
			}

		default:
			klog.Warningf("unrecognized packet type: %+v", pkt)
		}
	}
}

func (a *AgentClient) remoteToProxy(connID int64, ctx *connContext) {
	defer ctx.cleanup()

	var buf [1 << 12]byte
	resp := &client.Packet{
		Type: client.PacketType_DATA,
	}

	for {
		n, err := ctx.conn.Read(buf[:])
		klog.Infof("received %d bytes from remote for connID[%d]", n, connID)

		if err == io.EOF {
			klog.Info("connection EOF")
			return
		} else if err != nil {
			// Normal when receive a CLOSE_REQ
			klog.Warningf("connection read error: %v", err)
			return
		} else {
			resp.Payload = &client.Packet_Data{Data: &client.Data{
				Data:      buf[:n],
				ConnectID: connID,
			}}
			if err := a.stream.RetrySend(resp); err != nil {
				klog.Warningf("stream send error: %v", err)
			}
		}
	}
}

func (a *AgentClient) proxyToRemote(connID int64, ctx *connContext) {
	defer ctx.cleanup()

	for d := range ctx.dataCh {
		pos := 0
		for {
			n, err := ctx.conn.Write(d[pos:])
			if err == nil {
				klog.Infof("[connID: %d] write last %d data to remote", connID, n)
				break
			} else if n > 0 {
				klog.Infof("[connID: %d] write %d data to remote with error: %v", connID, n, err)
				pos += n
			} else {
				klog.Errorf("conn write error: %v", err)
				return
			}
		}
	}
}
