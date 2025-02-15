/*
 * canary-bot
 *
 * (C) 2022, Maximilian Schubert, Deutsche Telekom IT GmbH
 *
 * Deutsche Telekom IT GmbH and all other contributors /
 * copyright owners license this file to you under the Apache
 * License, Version 2.0 (the "License"); you may not use this
 * file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package mesh

import (
	"context"
	"strconv"
	"time"

	"github.com/telekom/canary-bot/data"
	h "github.com/telekom/canary-bot/helper"
	meshv1 "github.com/telekom/canary-bot/proto/mesh/v1"

	grpc_zap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

// MeshClient is the client for the mesh service
type MeshClient struct {
	conn   *grpc.ClientConn
	client meshv1.MeshServiceClient
}

// Join is used by the node to join the mesh network
func (m *Mesh) Join(targets []string) (bool, bool) {
	log := m.logger.Named("join-routine")
	var res *meshv1.JoinMeshResponse
	log.Debugw("Starting")

	// try to connect to one node in targets
	for index, target := range targets {
		log.Debugf("Index %+v Targets: %+v", index, targets)
		node := &meshv1.Node{Name: "", Target: target}

		err := m.initClient(node)
		if err != nil {
			m.logger.Debug("Could not connect to client, joinMesh request failed")
			if index != len(targets)-1 {
				log.Debugw("Trying next node", "error", err)
				continue
			}
			return false, true
		}

		// send join mesh request
		res, err = m.clients[GetId(node)].client.JoinMesh(
			context.Background(),
			&meshv1.Node{
				Name:   m.setupConfig.Name,
				Target: m.setupConfig.JoinAddress,
			})

		if err != nil {
			m.logger.Debug("Client connected, but joinMesh request failed")
			if index != len(targets)-1 {
				log.Debugw("Trying next node", "error", err)
				continue
			}
			return false, true
		}

		// check if name of node is unique in mesh response
		if !res.NameUnique {
			log.Debugw("Node name is not unique in mesh")
			return true, false
		}

		// save join-requested node as node in mesh
		node.Name = res.MyName
		m.database.SetNode(data.Convert(node, NODE_OK))

		log.Infow("Joined mesh", "name", node.Name, "target", node.Target)
		break
	}
	for _, node := range res.Nodes {
		if GetId(node) != GetId(&meshv1.Node{
			Name:   m.setupConfig.Name,
			Target: m.setupConfig.JoinAddress,
		}) {
			m.database.SetNode(data.Convert(node, NODE_OK))
		}
	}
	return true, true
}

func (m *Mesh) ping(node *meshv1.Node) error {
	log := m.logger.Named("ping-routine")
	err := m.initClient(node)
	if err != nil {
		log.Debugw("Could not connect to client")
		return err
	}
	_, err = m.clients[GetId(node)].client.Ping(
		context.Background(),
		&meshv1.Node{
			Name:   m.setupConfig.Name,
			Target: m.setupConfig.JoinAddress,
		})
	if err != nil {
		log.Debugw("Ping failed")
		return err
	}

	return nil
}

func (m *Mesh) NodeDiscovery(toNode *meshv1.Node, newNode *meshv1.Node) {
	log := m.logger.Named("discovery-routine")
	err := m.initClient(toNode)
	if err != nil {
		log.Warnw("Could not connect to client - skip Node Discover Request", "node", toNode.Name)
		return
	}
	_, err = m.clients[GetId(toNode)].client.NodeDiscovery(
		context.Background(),
		&meshv1.NodeDiscoveryRequest{
			NewNode: newNode,
			IAmNode: &meshv1.Node{
				Name:   m.setupConfig.Name,
				Target: m.setupConfig.ListenAddress + ":" + strconv.FormatInt(m.setupConfig.ListenPort, 10),
			},
		})
	if err != nil {
		log.Warnf("Could not start request to client - skip Node Discover Request", "node", toNode.Name, "error", err)
	}
	return
}

func (m *Mesh) pushSamples(node *meshv1.Node) error {
	log := m.logger.Named("sample-routine")
	err := m.initClient(node)
	if err != nil {
		log.Debugw("Could not connect to client")
		return err
	}

	var samples []*meshv1.Sample
	databaseSamples := m.database.GetSampleList()
	if len(databaseSamples) == 0 {
		log.Debugw("No samples found for push - will not push")
		return nil
	}
	for _, sample := range m.database.GetSampleList() {
		samples = append(samples, &meshv1.Sample{From: sample.From, To: sample.To, Key: sample.Key, Value: sample.Value, Ts: sample.Ts})
	}

	_, err = m.clients[GetId(node)].client.PushSamples(context.Background(), &meshv1.Samples{Samples: samples})
	if err != nil {
		log.Debugw("Could not send samples", "error", err)
		return err
	}
	return nil
}

func (m *Mesh) initClient(to *meshv1.Node) error {
	nodeId := GetId(to)
	log := m.logger.Named("client")
	log.Debugw("Init client")

	if m.setupConfig.DebugGrpc {
		grpc_zap.ReplaceGrpcLoggerV2(log.Named("grpc").Desugar())
	}

	if _, exists := m.clients[nodeId]; !exists {
		var opts []grpc.DialOption

		// TLS
		tlsCredentials, err := h.LoadClientTLSCredentials(m.setupConfig.CaCertPath, m.setupConfig.CaCert)
		if err != nil {
			log.Debugw("Cannot load TLS credentials - starting insecure connection", "error", err.Error())
			opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		} else {
			opts = append(opts, grpc.WithTransportCredentials(tlsCredentials))
		}

		// Timeout interceptor
		opts = append(opts, grpc.WithUnaryInterceptor(m.timeoutInterceptor))

		// dial
		conn, err := grpc.Dial(to.Target, opts...)
		if err != nil {
			log.Debugw("Dial error", "error", err)
			return err
		}

		client := meshv1.NewMeshServiceClient(conn)

		m.mu.Lock() // TODO has to be moved to the top?
		m.clients[nodeId] = &MeshClient{
			client: client,
			conn:   conn,
		}
		m.mu.Unlock()
	} else {
		log.Debugw("Client already existed")
	}
	return nil
}

func (m *Mesh) timeoutInterceptor(
	ctx context.Context,
	method string,
	req interface{},
	reply interface{},
	cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption,
) error {
	ctx, close := context.WithTimeout(context.Background(), m.routineConfig.RequestTimeout)
	defer close()
	// Calls the invoker to execute RPC
	err := invoker(ctx, method, req, reply, cc, opts...)
	return err
}

func (m *Mesh) closeClient(to *meshv1.Node) error {
	m.mu.Lock()
	err := m.clients[GetId(to)].conn.Close()
	if err != nil {
		return err
	}
	// remove client
	delete(m.clients, GetId(to))
	m.mu.Unlock()
	return nil
}

func (m *Mesh) Rtt() {
	log := m.logger.Named("rtt")
	log.Debugw("Starting RTT measurement")
	var opts []grpc.DialOption
	var rttStartH, rttStart, rttEnd time.Time

	nodes := m.database.GetRandomNodeListByState(NODE_OK, 1)
	if nodes == nil {
		log.Debugw("No Node suitable for RTT measurement")
		return
	}
	// select random node for RTT measurement
	node := nodes[0]
	log.Debugw("Node selected", "node", node.Name)
	// grpc logging
	if m.setupConfig.DebugGrpc {
		grpc_zap.ReplaceGrpcLoggerV2(log.Named("grpc").Desugar())
	}

	// TLS
	tlsCredentials, err := h.LoadClientTLSCredentials(m.setupConfig.CaCertPath, m.setupConfig.CaCert)
	if err != nil {
		log.Debugw("Cannot load TLS credentials - starting insecure connection", "error", err.Error())
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(tlsCredentials))
	}

	// blocking
	opts = append(opts, grpc.WithBlock())

	// start RTT with TCP handshake
	rttStartH = time.Now()
	// dial
	conn, err := grpc.Dial(node.Target, opts...)
	defer conn.Close()
	if err != nil {
		log.Debugw("Dial error", "error", err)
		return
	}

	client := meshv1.NewMeshServiceClient(conn)
	if err != nil {
		log.Debugw("Could not connect to client")
		return
	}

	// start RTT without TCP handshake
	rttStart = time.Now()

	// send request
	_, err = client.Rtt(context.Background(), &emptypb.Empty{})
	// end RTT
	rttEnd = time.Now()

	if err != nil {
		log.Debugw("RTT failed")
		return
	}
	log.Debugw("RTT succeeded")
	// RTT with handshake
	rttH := rttEnd.Sub(rttStartH)
	// RTT without handshake
	rtt := rttEnd.Sub(rttStart)

	// save metrics
	m.metrics.GetRtt().WithLabelValues(data.SampleName[data.RTT_TOTAL], node.Name).Observe(rttH.Seconds())
	m.metrics.GetRtt().WithLabelValues(data.SampleName[data.RTT_REQUEST], node.Name).Observe(rtt.Seconds())

	// save samples
	m.database.SetSample(
		&data.Sample{
			From:  m.setupConfig.Name,
			To:    node.Name,
			Key:   data.RTT_TOTAL,
			Value: strconv.FormatInt(rttH.Nanoseconds(), 10),
			Ts:    time.Now().Unix(),
		},
	)

	m.database.SetSample(
		&data.Sample{
			From:  m.setupConfig.Name,
			To:    node.Name,
			Key:   data.RTT_REQUEST,
			Value: strconv.FormatInt(rtt.Nanoseconds(), 10),
			Ts:    time.Now().Unix(),
		},
	)

	return
}
