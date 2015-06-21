package client

import (
	"github.com/golang/protobuf/proto"
	"golang.org/x/net/context"

	"github.com/obeattie/mercury"
	terrors "github.com/obeattie/typhon/errors"
	tmsg "github.com/obeattie/typhon/message"
)

// A Call is a convenient way to form a protobuf Request for an RPC call.
type Call struct {
	// Uid represents a unique identifier for this call; it is used.
	Uid string
	// Service to receive the call.
	Service string
	// Endpoint of the receiving service.
	Endpoint string
	// A protobuf Message which will be serialised to form the Payload of the request.
	Body proto.Message
	// Headers to send on the request (these may be augmented by the client).
	Headers map[string]string
	// Response is a protobuf Message into which the response's Payload should be unmarshaled.
	Response proto.Message
	// Context is a context for the request. This should nearly always be the parent request (if any).
	Context context.Context
}

func (c Call) marshaler() tmsg.Marshaler {
	return tmsg.ProtoMarshaler()
}

// Request yields a Request formed from this Call
func (c Call) Request() (mercury.Request, error) {
	req := mercury.NewRequest()
	req.SetService(c.Service)
	req.SetEndpoint(c.Endpoint)
	req.SetHeaders(c.Headers)
	if c.Context != nil {
		req.SetContext(c.Context)
	}
	if c.Body != nil {
		req.SetBody(c.Body)
		if err := c.marshaler().MarshalBody(req); err != nil {
			terr := terrors.Wrap(err)
			terr.Code = terrors.ErrBadRequest
			return nil, terr
		}
	}
	return req, nil
}