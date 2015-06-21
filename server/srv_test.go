package server

import (
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/obeattie/mercury"
	"github.com/obeattie/mercury/testproto"
	"github.com/obeattie/mercury/transport"
	terrors "github.com/obeattie/typhon/errors"
	tmsg "github.com/obeattie/typhon/message"
	"github.com/obeattie/typhon/mock"
	pe "github.com/obeattie/typhon/proto/error"
	"github.com/obeattie/typhon/rabbit"
)

const testServiceName = "service.server-example"

func TestServerSuite_MockTransport(t *testing.T) {
	suite.Run(t, &serverSuite{
		TransF: func() transport.Transport {
			return mock.NewTransport()
		},
	})
}

func TestServerSuite_RabbitTransport(t *testing.T) {
	suite.Run(t, &serverSuite{
		TransF: func() transport.Transport {
			return rabbit.NewTransport()
		},
	})
}

type serverSuite struct {
	suite.Suite
	TransF func() transport.Transport
	trans  transport.Transport
	server Server
}

func (suite *serverSuite) SetupSuite() {
	// Share a single transport between all tests (which each use a different server). This deliberately tests the
	// underlying transport's ability to connect and disconnect a service while leaving the connection open.
	suite.trans = suite.TransF()
	select {
	case <-suite.trans.Ready():
	case <-time.After(5 * time.Second):
		panic("transport not ready")
	}
}

func (suite *serverSuite) SetupTest() {
	suite.server = NewServer(testServiceName)
	suite.server.Start(suite.trans)
}

func (suite *serverSuite) TearDownTest() {
	suite.server.Stop()
	suite.server = nil
}

func (suite *serverSuite) TearDownSuite() {
	suite.trans.Tomb().Killf("Test ending")
	suite.trans.Tomb().Wait()
	suite.trans = nil
}

// TestRouting verifies a registered endpoint receives messages destined for it, and that responses are sent
// appropriately
func (suite *serverSuite) TestRouting() {
	srv := suite.server
	srv.AddEndpoints(Endpoint{
		Name:     "dummy",
		Request:  new(testproto.DummyRequest),
		Response: new(testproto.DummyResponse),
		Handler: func(req mercury.Request) (mercury.Response, error) {
			request := req.Body().(*testproto.DummyRequest)
			rsp := req.Response(&testproto.DummyResponse{
				Pong: request.Ping,
			})
			rsp.SetHeader("X-Ping-Pong", request.Ping)
			return rsp, nil
		}})

	req := mercury.NewRequest()
	req.SetService(testServiceName)
	req.SetEndpoint("dummy")
	req.SetBody(&testproto.DummyRequest{
		Ping: "routing"})
	suite.Assert().NoError(tmsg.ProtoMarshaler().MarshalBody(req))

	rsp, err := suite.trans.Send(req, time.Second)
	suite.Assert().NoError(err)
	suite.Assert().NotNil(rsp)
	suite.Assert().Equal(req.Service(), rsp.Service())
	suite.Assert().Equal(req.Endpoint(), rsp.Endpoint())

	suite.Assert().NoError(tmsg.ProtoUnmarshaler(new(testproto.DummyResponse)).UnmarshalPayload(rsp))
	suite.Assert().NotNil(rsp.Body())
	suite.Assert().IsType(new(testproto.DummyResponse), rsp.Body())
	response := rsp.Body().(*testproto.DummyResponse)
	suite.Assert().Equal("routing", response.Pong)
	suite.Assert().Equal("routing", rsp.Headers()["X-Ping-Pong"])
}

// TestErrorResponse tests that errors are serialised and returned to callers appropriately (as we are using the
// transport directly here, we actually get a response containing an error, not a transport error)
func (suite *serverSuite) TestErrorResponse() {}

// TestNilResponse tests that a nil response correctly returns a Response with a nil body to the caller
func (suite *serverSuite) TestNilResponse() {}

// TestEndpointNotFound tests that a Bad Request error is correctly returned on receiving an event for an unknown
// endpoing
func (suite *serverSuite) TestEndpointNotFound() {
	req := mercury.NewRequest()
	req.SetService(testServiceName)
	req.SetEndpoint("dummy")
	req.SetBody(&testproto.DummyRequest{
		Ping: "routing"})
	suite.Assert().NoError(tmsg.ProtoMarshaler().MarshalBody(req))

	rsp_, err := suite.trans.Send(req, time.Second)
	rsp := mercury.FromTyphonResponse(rsp_)
	suite.Assert().NoError(err)
	suite.Assert().NotNil(rsp)
	suite.Assert().True(rsp.IsError())

	suite.Assert().NoError(tmsg.ProtoUnmarshaler(new(pe.Error)).UnmarshalPayload(rsp))
	suite.Assert().IsType(new(pe.Error), rsp.Body())
	terr := terrors.Unmarshal(rsp.Body().(*pe.Error))
	suite.Assert().Equal(terrors.ErrBadRequest, terr.Code)
	suite.Assert().Contains("Endpoint not found", terr.Error())
}

// TestRegisteringInvalidEndpoint tests that appropriate panics are raised when registering invalid Endpoints
func (suite *serverSuite) TestRegisteringInvalidEndpoint() {}

// TestRoutingParallel sends a bunch of requests in parallel to different endpoints and checks that the responses match
// correctly. 200 workers, 100 requests each.
func (suite *serverSuite) TestRoutingParallel() {
	if testing.Short() {
		suite.T().Skip("Skipping TestRoutingParallel in short mode")
	}

	names := [...]string{"1", "2", "3"}
	srv := suite.server
	srv.AddEndpoints(
		Endpoint{
			Name:     names[0],
			Request:  new(testproto.DummyRequest),
			Response: new(testproto.DummyResponse),
			Handler: func(req mercury.Request) (mercury.Response, error) {
				return req.Response(&testproto.DummyResponse{Pong: names[0]}), nil
			}},
		Endpoint{
			Name:     names[1],
			Request:  new(testproto.DummyRequest),
			Response: new(testproto.DummyResponse),
			Handler: func(req mercury.Request) (mercury.Response, error) {
				return req.Response(&testproto.DummyResponse{Pong: names[1]}), nil
			}},
		Endpoint{
			Name:     names[2],
			Request:  new(testproto.DummyRequest),
			Response: new(testproto.DummyResponse),
			Handler: func(req mercury.Request) (mercury.Response, error) {
				return req.Response(&testproto.DummyResponse{Pong: names[2]}), nil
			}})

	workers := 200
	wg := sync.WaitGroup{}
	wg.Add(workers)
	unmarshaler := tmsg.ProtoUnmarshaler(new(testproto.DummyResponse))
	work := func(i int) {
		defer wg.Done()
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		ep := names[rng.Int()%len(names)]
		for i := 0; i < 100; i++ {
			req := mercury.NewRequest()
			req.SetService(testServiceName)
			req.SetEndpoint(ep)
			req.SetBody(&testproto.DummyRequest{})
			suite.Assert().NoError(tmsg.ProtoMarshaler().MarshalBody(req))

			rsp, err := suite.trans.Send(req, time.Second)
			suite.Assert().NoError(err)
			suite.Assert().NotNil(rsp)
			suite.Assert().NoError(unmarshaler.UnmarshalPayload(rsp))
			response := rsp.Body().(*testproto.DummyResponse)
			suite.Assert().Equal(ep, response.Pong)
		}
	}

	for i := 0; i < workers; i++ {
		go work(i)
	}
	wg.Wait()
}

// TestJSONResponse verifies that a request sent with JSON content and an accepting JSON response, does in fact yield a
// JSON response (from a proto handler)
func (suite *serverSuite) TestJSONResponse() {
	srv := suite.server
	srv.AddEndpoints(Endpoint{
		Name:     "dummy",
		Request:  new(testproto.DummyRequest),
		Response: new(testproto.DummyResponse),
		Handler: func(req mercury.Request) (mercury.Response, error) {
			request := req.Body().(*testproto.DummyRequest)
			return req.Response(&testproto.DummyResponse{
				Pong: request.Ping,
			}), nil
		}})

	req := mercury.NewRequest()
	req.SetService(testServiceName)
	req.SetEndpoint("dummy")
	req.SetBody(map[string]string{
		"ping": "json"})
	req.SetHeader("Accept", "application/json")
	suite.Assert().NoError(tmsg.JSONMarshaler().MarshalBody(req))

	rsp, err := suite.trans.Send(req, time.Second)
	suite.Assert().NoError(err)
	suite.Assert().NotNil(rsp)

	suite.Assert().Equal("application/json", rsp.Headers()["Content-Type"])
	suite.Assert().Equal(`{"pong":"json"}`, string(rsp.Payload()))
}