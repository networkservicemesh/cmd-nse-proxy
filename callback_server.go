// Copyright (c) 2020 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/ptypes/empty"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/networkservicemesh/api/pkg/api/networkservice"
	api_registry "github.com/networkservicemesh/api/pkg/api/registry"
	"github.com/networkservicemesh/sdk/pkg/registry/core/next"
	"github.com/networkservicemesh/sdk/pkg/tools/callback"
)

type callbackClientEntry struct {
	server *grpc.Server
	client callback.Client
}
type callbackRegistryServer struct {
	clientCC *grpc.ClientConn
	server   callback.Server

	servers map[string]*callbackClientEntry
	sync.Mutex
	source *workloadapi.X509Source
}

type nseProxyServer struct {
	endpointCC *grpc.ClientConn
	endpoint   networkservice.NetworkServiceClient
}

func (n *nseProxyServer) Request(ctx context.Context, request *networkservice.NetworkServiceRequest) (*networkservice.Connection, error) {
	return n.endpoint.Request(ctx, request)
}

func (n *nseProxyServer) Close(ctx context.Context, connection *networkservice.Connection) (*empty.Empty, error) {
	return n.endpoint.Close(ctx, connection)
}
func (n *nseProxyServer) MonitorConnections(selector *networkservice.MonitorScopeSelector, server networkservice.MonitorConnection_MonitorConnectionsServer) error {
	logrus.Infof("Started monitoring Client %v", selector)
	nsmMonitorClient := networkservice.NewMonitorConnectionClient(n.endpointCC)
	client, err := nsmMonitorClient.MonitorConnections(server.Context(), selector)
	if err != nil {
		return err
	}
	for {
		select {
		case <-server.Context().Done():
			return server.Context().Err()
		default:
			{
				evt, recverr := client.Recv()
				if recverr != nil {
					return recverr
				}
				logrus.Infof("MONITORING Event: %v", evt)
				sendErr := server.Send(evt)
				if sendErr != nil {
					return sendErr
				}
			}
		}
	}
}

func (c *callbackRegistryServer) Register(ctx context.Context, serviceEndpoint *api_registry.NetworkServiceEndpoint) (*api_registry.NetworkServiceEndpoint, error) {
	logrus.Infof("Proxy Register Endpoint: %v", serviceEndpoint)

	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsconfig.MTLSServerConfig(c.source, c.source, tlsconfig.AuthorizeAny()))))

	logrus.Infof("Dial back to source Endpoint: %v", serviceEndpoint.Url)

	dialCtx, cancel := context.WithTimeout(ctx, 240*time.Second)
	defer cancel()
	// Connect to endpoint with callback
	endpointCC, err := grpc.DialContext(dialCtx, serviceEndpoint.Url, c.server.WithCallbackDialer(), grpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	proxyEndpoint := &nseProxyServer{
		endpointCC: endpointCC,
		endpoint:   networkservice.NewNetworkServiceClient(endpointCC),
	}
	networkservice.RegisterNetworkServiceServer(grpcServer, proxyEndpoint)
	networkservice.RegisterMonitorConnectionServer(grpcServer, proxyEndpoint)

	// Serve callbacks
	callbackClient := callback.NewClient(c.clientCC, grpcServer)
	// Pass same endpoint Url to be handled by us
	callbackClient.Serve(WithCallbackEndpointID(context.Background(), strings.Replace(serviceEndpoint.Url, "callback:", "", 1)))

	c.Lock()
	c.servers[serviceEndpoint.Url] = &callbackClientEntry{
		server: grpcServer,
		client: callbackClient,
	}
	c.Unlock()

	logrus.Infof("Calling Register on NSMGR with %v", serviceEndpoint)
	return next.NetworkServiceEndpointRegistryServer(ctx).Register(ctx, serviceEndpoint)
}

func (c *callbackRegistryServer) Find(query *api_registry.NetworkServiceEndpointQuery, server api_registry.NetworkServiceEndpointRegistry_FindServer) error {
	// Just proxy request
	return next.NetworkServiceEndpointRegistryServer(server.Context()).Find(query, server)
}

func (c *callbackRegistryServer) Unregister(ctx context.Context, serviceEndpoint *api_registry.NetworkServiceEndpoint) (*empty.Empty, error) {
	emp, err := next.NetworkServiceEndpointRegistryServer(ctx).Unregister(ctx, serviceEndpoint)
	c.Lock()
	s, ok := c.servers[serviceEndpoint.Url]
	if ok {
		s.client.Stop()
		s.server.Stop()
	}
	c.Unlock()
	return emp, err
}
