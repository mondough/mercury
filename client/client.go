package client

import (
	"reflect"
	"sync"
	"time"

	log "github.com/cihub/seelog"
	"github.com/golang/protobuf/proto"

	"github.com/mondough/mercury"
	"github.com/mondough/mercury/transport"
	terrors "github.com/mondough/typhon/errors"
	tmsg "github.com/mondough/typhon/message"
	tperrors "github.com/mondough/typhon/proto/error"
)

const defaultTimeout = 10 * time.Second

var (
	defaultMiddleware  []ClientMiddleware
	defaultMiddlewareM sync.RWMutex
)

type clientCall struct {
	uid      string           // unique identifier within a client
	req      mercury.Request  // may be nil in the case of a request marshaling failure
	rsp      mercury.Response // set when a response is received
	rspProto proto.Message    // shared with rsp when unmarshaled
	err      *terrors.Error   // execution error or unmarshalled (remote) error
}

type client struct {
	sync.RWMutex
	calls      map[string]clientCall // uid: call
	doneC      chan struct{}         // closed when execution has finished; immutable
	execC      chan struct{}         // closed when execution begins; immutable
	execOnce   sync.Once             // ensures execution only happens once
	timeout    time.Duration         // default: defaultTimeout
	trans      transport.Transport   // defaults to the global default
	middleware []ClientMiddleware    // applied in-order for requests, reverse-order for responses
}

func NewClient() Client {
	defaultMiddlewareM.RLock()
	middleware := defaultMiddleware
	defaultMiddlewareM.RUnlock()

	return &client{
		calls:      make(map[string]clientCall),
		doneC:      make(chan struct{}),
		execC:      make(chan struct{}),
		timeout:    defaultTimeout,
		middleware: middleware,
	}
}

func SetDefaultMiddleware(middleware []ClientMiddleware) {
	defaultMiddlewareM.Lock()
	defer defaultMiddlewareM.Unlock()
	defaultMiddleware = middleware
}

func (c *client) transport() transport.Transport {
	// Callers must hold (at least) a read lock
	if c.trans != nil {
		return c.trans
	} else {
		return transport.DefaultTransport()
	}
}

func (c *client) addCall(cc clientCall) {
	select {
	case <-c.execC:
		log.Warn("[Mercury:Client] Request added after client execution; discarding")
		return
	default:
	}

	c.Lock()
	defer c.Unlock()
	c.calls[cc.uid] = cc
}

func (c *client) Add(cl Call) Client {
	cc := clientCall{
		uid:      cl.Uid,
		rspProto: cl.Response,
	}
	req, err := cl.Request()
	if err != nil {
		cc.err = terrors.Wrap(err)
	} else {
		cc.req = req
	}
	c.addCall(cc)
	return c
}

func (c *client) AddRequest(uid string, req mercury.Request) Client {
	c.addCall(clientCall{
		uid:      uid,
		req:      req,
		rspProto: nil,
	})
	return c
}

func (c *client) Errors() ErrorSet {
	c.RLock()
	defer c.RUnlock()
	errs := ErrorSet(nil)
	for uid, call := range c.calls {
		if call.err != nil {
			err := call.err
			err.PrivateContext[errUidField] = uid
			err.PrivateContext[errServiceField] = call.req.Service()
			err.PrivateContext[errEndpointField] = call.req.Endpoint()
			errs = append(errs, err)
		}
	}
	return errs
}

// performCall executes a single Call, unmarshals the response (if there is a response proto), and pushes the updted
// clientCall down the response channel
func (c *client) performCall(call clientCall, middleware []ClientMiddleware, trans transport.Transport,
	timeout time.Duration, completion chan<- clientCall) {

	// Apply request middleware
	req := call.req
	for _, md := range middleware {
		req = md.ProcessClientRequest(req)
	}

	log.Debugf("[Mercury:Client] Sending request to %s/%s…", req.Service(), req.Endpoint())

	rsp_, err := trans.Send(req, timeout)
	if err != nil {
		call.err = terrors.Wrap(err)
	} else {
		rsp := mercury.FromTyphonResponse(rsp_)

		// Servers set header Content-Error: 1 when sending errors. For those requests, unmarshal the error, leaving the
		// call's response nil
		if rsp.IsError() {
			err := new(tperrors.Error)
			if unmarshalErr := proto.Unmarshal(rsp.Payload(), err); unmarshalErr != nil {
				call.err = terrors.Wrap(unmarshalErr)
				call.err.Code = terrors.ErrBadResponse
			} else {
				call.err = terrors.Unmarshal(err)
			}

			// Set the response Body to a nil – but typed – interface to avoid type conversion panics if Body
			// properties are accessed in spite of the error
			// Relevant: http://golang.org/doc/faq#nil_error
			if call.rspProto != nil {
				bodyT := reflect.TypeOf(call.rspProto)
				rsp.SetBody(reflect.New(bodyT.Elem()).Interface())
			}

		} else if call.rspProto != nil {
			rsp.SetBody(call.rspProto)
			unmarshaler := tmsg.ProtoUnmarshaler(call.rspProto)
			if err := unmarshaler.UnmarshalPayload(rsp); err != nil {
				log.Warnf("[Mercury:Client] Could not unmarshal response: %v", err)
				call.err = terrors.Wrap(err)
				call.err.Code = terrors.ErrBadResponse
			}
		}

		call.rsp = rsp
	}

	// Apply response/error middleware (in reverse order)
	for i := len(middleware) - 1; i >= 0; i-- {
		mw := middleware[i]
		if call.err != nil {
			mw.ProcessClientError(call.err, call.req)
		} else {
			call.rsp = mw.ProcessClientResponse(call.rsp, call.req)
		}
	}

	completion <- call
}

// exec actually executes the requests; called by Go() within a sync.Once.
func (c *client) exec() {
	defer close(c.doneC)

	c.Lock()
	timeout := c.timeout
	calls := c.calls // We don't need to make a copy as calls cannot be mutated once execution begins
	trans := c.transport()
	middleware := c.middleware
	c.Unlock()

	completedCallsC := make(chan clientCall, len(calls))
	for _, call := range calls {
		if call.err != nil {
			completedCallsC <- call
			continue
		} else if trans == nil {
			call.err = terrors.InternalService("Client has no transport")
			completedCallsC <- call
			continue
		}
		go c.performCall(call, middleware, trans, timeout, completedCallsC)
	}

	// Collect completed calls into a new map
	completedCalls := make(map[string]clientCall, cap(completedCallsC))
	for i := 0; i < cap(completedCallsC); i++ {
		call := <-completedCallsC
		completedCalls[call.uid] = call
	}
	close(completedCallsC)

	c.Lock()
	defer c.Unlock()
	c.calls = completedCalls
}

func (c *client) Go() Client {
	c.execOnce.Do(func() {
		close(c.execC)
		go c.exec()
	})
	return c
}

func (c *client) WaitC() <-chan struct{} {
	return c.doneC
}

func (c *client) Wait() Client {
	<-c.WaitC()
	return c
}

func (c *client) Execute() Client {
	return c.Go().Wait()
}

func (c *client) Response(uid string) mercury.Response {
	c.RLock()
	defer c.RUnlock()
	if call, ok := c.calls[uid]; ok {
		return call.rsp
	}
	return nil
}

func (c *client) SetTimeout(to time.Duration) Client {
	c.Lock()
	defer c.Unlock()
	c.timeout = to
	return c
}

func (c *client) SetTransport(trans transport.Transport) Client {
	c.Lock()
	defer c.Unlock()
	c.trans = trans
	return c
}

func (c *client) Middleware() []ClientMiddleware {
	c.RLock()
	defer c.RUnlock()
	return c.middleware
}

func (c *client) AddMiddleware(md ClientMiddleware) Client {
	c.Lock()
	defer c.Unlock()
	c.middleware = append(c.middleware, md)
	return c
}

func (c *client) SetMiddleware(mds []ClientMiddleware) Client {
	c.Lock()
	defer c.Unlock()
	c.middleware = mds
	return c
}
