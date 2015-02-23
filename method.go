package kite

import (
	"sync"
	"time"

	"github.com/juju/ratelimit"
)

// MethodHandling defines how to handle chaining of kite.Handler middlewares.
// An error breaks the chain regardless of what handling is used. Note that all
// Pre and Post handlers are executed regardless the handling logic, only the
// return paramater is defined by the handling mode.
type MethodHandling int

const (
	// ReturnMethod returns main method's response. This is the standard default.
	ReturnMethod MethodHandling = iota

	// ReturnFirst returns the first non-nil response.
	ReturnFirst

	// ReturnLatest returns the latest response (waterfall behaviour)
	ReturnLatest
)

// Objects implementing the Handler interface can be registered to a method.
// The returned result must be marshalable with json package.
type Handler interface {
	ServeKite(*Request) (result interface{}, err error)
}

// HandlerFunc is a type adapter to allow the use of ordinary functions as
// Kite handlers. If h is a function with the appropriate signature,
// HandlerFunc(h) is a Handler object that calls h.
type HandlerFunc func(*Request) (result interface{}, err error)

// ServeKite calls h(r)
func (h HandlerFunc) ServeKite(r *Request) (interface{}, error) {
	return h(r)
}

// Method defines a method and the Handler it is bind to. By default
// "ReturnMethod" handling is used.
type Method struct {
	// name is the method name. Unnamed methods can exist
	name string

	// handler contains the related Handler for the given method
	handler      Handler   // handler is the base handler, the response of it is returned as the final
	preHandlers  []Handler // a list of handlers that are executed before the main handler
	postHandlers []Handler // a list of handlers that are executed after the main handler

	// authenticate defines if a given authenticator function is enabled for
	// the given auth type in the request.
	authenticate bool

	// handling defines how to handle chaining of kite.Handler middlewares.
	handling MethodHandling

	// initialized is used to indicate whether all pre and post handlers are
	// initialized.
	initialized bool

	// bucket is used for throttling the method by certain rule
	bucket *ratelimit.Bucket

	mu sync.Mutex // protects handler slices
}

// addHandle is an internal method to add a handler
func (k *Kite) addHandle(method string, handler Handler) *Method {
	authenticate := true
	if k.Config.DisableAuthentication {
		authenticate = false
	}

	m := &Method{
		name:         method,
		handler:      handler,
		preHandlers:  make([]Handler, 0),
		postHandlers: make([]Handler, 0),
		authenticate: authenticate,
		handling:     k.MethodHandling,
	}

	k.handlers[method] = m
	return m
}

// DisableAuthentication disables authentication check for this method.
func (m *Method) DisableAuthentication() *Method {
	m.authenticate = false
	return m
}

// Throttle throttles the method for each incoming request. The throttle
// algorithm is based on token bucket implementation:
// http://en.wikipedia.org/wiki/Token_bucket. Rate determines the number of
// request which are allowed per frequency. Example: A rate of 50 and frequency
// of two minutes means that the method can receive 50 request in two minutes,
// if there is more requests in this two minutes they will be rejected. Another
// example would be a rate of 30 and frequency of one second. This basically
// means the method is limited to 30 requests per second.
func (m *Method) Throttle(rate int64, frequency time.Duration) *Method {
	// don't do anything if the bucket is initialized already
	if m.bucket != nil {
		return m
	}

	m.bucket = ratelimit.NewBucketWithQuantum(
		frequency, // interval
		rate,      // capacity
		rate,      // token per interval
	)

	return m
}

// PreHandler adds a new kite handler which is executed before the method.
func (m *Method) PreHandle(handler Handler) *Method {
	m.preHandlers = append(m.preHandlers, handler)
	return m
}

// PreHandlerFunc adds a new kite handlerfunc which is executed before the
// method.
func (m *Method) PreHandleFunc(handler HandlerFunc) *Method {
	return m.PreHandle(handler)
}

// PostHandle adds a new kite handler which is executed after the method.
func (m *Method) PostHandle(handler Handler) *Method {
	m.postHandlers = append(m.postHandlers, handler)
	return m
}

// PostHandlerFunc adds a new kite handlerfunc which is executed before the
// method.
func (m *Method) PostHandleFunc(handler HandlerFunc) *Method {
	return m.PostHandle(handler)
}

// Handle registers the handler for the given method. The handler is called
// when a method call is received from a Kite.
func (k *Kite) Handle(method string, handler Handler) *Method {
	return k.addHandle(method, handler)
}

// HandleFunc registers a handler to run when a method call is received from a
// Kite. It returns a *Method option to further modify certain options on a
// method call
func (k *Kite) HandleFunc(method string, handler HandlerFunc) *Method {
	return k.addHandle(method, handler)
}

// PreHandle registers an handler which is executed before a kite.Handler
// method is executed. Calling PreHandle multiple times registers multiple
// handlers. A non-error return triggers the execution of the next handler. The
// execution order is FIFO.
func (k *Kite) PreHandle(handler Handler) {
	k.preHandlers = append(k.preHandlers, handler)
}

// PreHandleFunc is the same as PreHandle. It accepts a HandlerFunc.
func (k *Kite) PreHandleFunc(handler HandlerFunc) {
	k.PreHandle(handler)
}

// PostHandle registers an handler which is executed after a kite.Handler
// method is executed. Calling PostHandler multiple times registers multiple
// handlers. A non-error return triggers the execution of the next handler. The
// execution order is FIFO.
func (k *Kite) PostHandle(handler Handler) {
	k.postHandlers = append(k.postHandlers, handler)
}

// PostHandleFunc is the same as PostHandle. It accepts a HandlerFunc.
func (k *Kite) PostHandleFunc(handler HandlerFunc) {
	k.PostHandle(handler)
}

func (m *Method) ServeKite(r *Request) (interface{}, error) {
	var firstResp interface{}
	var resp interface{}
	var err error

	// first execute preHandlers. make a copy of the handler to avoid race
	// conditions
	m.mu.Lock()
	preHandlers := make([]Handler, len(m.preHandlers))
	for i, handler := range m.preHandlers {
		preHandlers[i] = handler

	}
	m.mu.Unlock()

	for _, handler := range preHandlers {
		resp, err = handler.ServeKite(r)
		if err != nil {
			return nil, err
		}

		if m.handling == ReturnFirst && resp != nil && firstResp == nil {
			firstResp = resp
		}
	}

	preHandlers = nil // garbage collect it

	// now call our base handler
	resp, err = m.handler.ServeKite(r)
	if err != nil {
		return nil, err
	}

	// also save it dependent on the handling mechanism
	methodResp := resp

	if m.handling == ReturnFirst && resp != nil && firstResp == nil {
		firstResp = resp
	}

	// and finally return our postHandlers
	m.mu.Lock()
	postHandlers := make([]Handler, len(m.postHandlers))
	for i, handler := range m.postHandlers {
		postHandlers[i] = handler
	}
	m.mu.Unlock()

	for _, handler := range postHandlers {
		resp, err = handler.ServeKite(r)
		if err != nil {
			return nil, err
		}

		if m.handling == ReturnFirst && resp != nil && firstResp == nil {
			firstResp = resp
		}
	}

	postHandlers = nil // garbage collect it

	switch m.handling {
	case ReturnMethod:
		return methodResp, nil
	case ReturnFirst:
		return firstResp, nil
	case ReturnLatest:
		return resp, nil
	}

	return resp, nil
}
