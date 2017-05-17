// Package fastroute is standard http.Handler based high performance HTTP request router.
//
// A trivial example is:
//
//  package main
//
//  import (
//      "fmt"
//      "log"
//      "net/http"
//      "github.com/DATA-DOG/fastroute"
//  )
//
//  func Index(w http.ResponseWriter, r *http.Request) {
//      fmt.Fprint(w, "Welcome!\n")
//  }
//
//  func Hello(w http.ResponseWriter, r *http.Request) {
//      fmt.Fprintf(w, "hello, %s!\n", fastroute.Parameters(r).ByName("name"))
//  }
//
//  func main() {
//      log.Fatal(http.ListenAndServe(":8080", fastroute.New(
//          fastroute.Route("/", Index),
//          fastroute.Route("/hello/:name", Hello),
//      )))
//  }
//
// The router can be composed of fastroute.Router interface, which shares
// the same http.Handler interface. This package provides only this orthogonal
// interface as a building block.
//
// It also provides path pattern matching in order to construct dynamic routes
// having named Params available from http.Request at zero allocation cost.
// You can extract path parameters from request this way:
//
//  params := fastroute.Parameters(request) // request - *http.Request
//  fmt.Println(params.ByName("id"))
//
// The registered path, against which the router matches incoming requests, can
// contain two types of parameters:
//  Syntax    Type
//  :name     named parameter
//  *name     catch-all parameter
//
// Named parameters are dynamic path segments. They match anything until the
// next '/' or the path end:
//  Path: /blog/:category/:post
//
//  Requests:
//   /blog/go/request-routers            match: category="go", post="request-routers"
//   /blog/go/request-routers/           no match
//   /blog/go/                           no match
//   /blog/go/request-routers/comments   no match
//
// Catch-all parameters match anything until the path end, including the
// directory index (the '/' before the catch-all). Since they match anything
// until the end, catch-all parameters must always be the final path element.
//  Path: /files/*filepath
//
//  Requests:
//   /files/                             match: filepath="/"
//   /files/LICENSE                      match: filepath="/LICENSE"
//   /files/templates/article.html       match: filepath="/templates/article.html"
//   /files                              no match
//
package fastroute

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Parameters returns all path parameters for given
// request.
//
// If there were no parameters and route is static
// then empty parameter slice is returned.
func Parameters(req *http.Request) Params {
	if p := parameterized(req); p != nil {
		return p.params
	}
	return emptyParams
}

// Pattern gives matched route path pattern
// for this request.
//
// If request parameters were already flushed,
// meaning - it was either served or recycled
// manually, then empty string will be returned.
func Pattern(req *http.Request) string {
	if p := parameterized(req); p != nil {
		return p.pattern
	}
	return ""
}

// Recycle resets named parameters
// if they were assigned to the request.
//
// When using Router.Match(http.Request) func,
// parameters will be flushed only if matched
// http.Handler is served.
//
// If the purpose is just to test Router
// whether it matches or not, without serving
// matched handler, then this method should
// be invoked to prevent leaking parameters.
//
// If the route is not matched and handler is nil,
// then parameters will not be allocated, same
// as for static paths.
func Recycle(req *http.Request) {
	if p := parameterized(req); p != nil {
		p.reset(req)
	}
}

// Params is a slice of key value pairs, as extracted from
// the http.Request served by Router.
//
// The slice is ordered, the first URL parameter is also the first slice value.
// It is therefore safe to read values by the index.
type Params []struct{ Key, Value string }

// ByName returns the value of the first Param which key matches the given name.
// If no matching param is found, an empty string is returned.
func (ps Params) ByName(name string) string {
	for i := range ps {
		if ps[i].Key == name {
			return ps[i].Value
		}
	}
	return ""
}

// Router interface is robust and nothing more than
// http.Handler. It simply extends it with one extra method to Match
// http.Handler from http.Request and that allows to chain it
// until a handler is matched.
//
// Match func should return handler or nil.
type Router interface {
	http.Handler

	// Match should return nil if request
	// cannot be matched. When ServeHTTP is
	// invoked and handler is nil, it will
	// serve http.NotFoundHandler
	//
	// Note, if the router is matched and it has
	// path parameters - then it must be served
	// in order to release allocated parameters
	// back to the pool. Otherwise you will leak
	// parameters, which you can also salvage by
	// calling Recycle on http.Request
	Match(*http.Request) http.Handler
}

// RouterFunc type is an adapter to allow the use of
// ordinary functions as Routers. If f is a function
// with the appropriate signature, RouterFunc(f) is a
// Router that calls f.
type RouterFunc func(*http.Request) http.Handler

// Match calls f(r).
func (rf RouterFunc) Match(r *http.Request) http.Handler {
	return rf(r)
}

// ServeHTTP calls f(w, r).
func (rf RouterFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := rf(r); h != nil {
		h.ServeHTTP(w, r)
	} else {
		http.NotFound(w, r)
	}
}

// New creates Router combined of given routes.
// It attempts to match all routes in order, the first
// matched route serves the request.
//
// Users may sort routes the way he prefers, or add
// dynamic sorting goroutine, which calculates order
// based on hits.
func New(routes ...Router) Router {
	return RouterFunc(func(r *http.Request) http.Handler {
		var found http.Handler
		for _, router := range routes {
			if found = router.Match(r); found != nil {
				break
			}
		}
		return found
	})
}

// Route creates Router which attempts
// to match given path to handler.
//
// Handler is a standard http.Handler which
// may be accepted in the following formats:
//  http.Handler
//  func(http.ResponseWriter, *http.Request)
//
// Static paths will be simply matched to
// the request URL. While paths having named
// parameters will be matched by segment. And
// bind matched named parameters to http.Request.
//
// When dynamic path is matched, it must be served
// in order to salvage allocated named parameters.
func Route(path string, handler interface{}) Router {
	p := "/" + strings.TrimLeft(path, "/")

	var h http.Handler = nil
	switch t := handler.(type) {
	case http.HandlerFunc:
		h = t
	case func(http.ResponseWriter, *http.Request):
		h = http.HandlerFunc(t)
	default:
		panic(fmt.Sprintf("not a handler given: %T - %+v", t, t))
	}

	// maybe static route
	if strings.IndexAny(p, ":*") == -1 {
		ps := &parameters{params: emptyParams, pattern: p}
		return RouterFunc(func(r *http.Request) http.Handler {
			if compareFunc(p, r.URL.Path) {
				ps.wrap(r)
				return h
			}
			return nil
		})
	}

	// prepare and validate pattern segments to match
	segments := strings.Split(strings.Trim(p, "/"), "/")
	for i := 0; i < len(segments); i++ {
		seg := segments[i]
		segments[i] = "/" + seg
		if pos := strings.IndexAny(seg, ":*"); pos == -1 {
			continue
		} else if pos != 0 {
			panic("special param matching signs, must follow after slash: " + p)
		} else if len(seg)-1 == pos {
			panic("param must be named after sign: " + p)
		} else if seg[0] == '*' && i+1 != len(segments) {
			panic("match all, must be the last segment in pattern: " + p)
		} else if strings.IndexAny(seg[1:], ":*") != -1 {
			panic("only one param per segment: " + p)
		}
	}
	ts := p[len(p)-1] == '/' // whether we need to match trailing slash

	// pool for parameters
	num := strings.Count(p, ":") + strings.Count(p, "*")
	pool := sync.Pool{}
	pool.New = func() interface{} {
		return &parameters{params: make(Params, 0, num), pool: &pool, pattern: p}
	}

	// extend handler in order to salvage parameters
	handle := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		if p := parameterized(r); p != nil {
			p.reset(r)
		}
	})

	// dynamic route matcher
	return RouterFunc(func(r *http.Request) http.Handler {
		p := pool.Get().(*parameters)
		if match(segments, r.URL.Path, &p.params, ts) {
			p.wrap(r)
			return handle
		}
		p.reset(r)
		return nil
	})
}

// ComparesPathWith allows to use custom static path segment
// comparison func.
//
// By default it uses case sensitive comparison, but
// it can be overriden with for example strings.EqualFold
// to have case insensitive match.
//
// Note that, if application uses more than one router,
// it might conflict when applied concurrently.
func ComparesPathWith(router Router, cmp func(s1, s2 string) bool) Router {
	return RouterFunc(func(req *http.Request) http.Handler {
		compareFunc = cmp
		handler := router.Match(req)
		compareFunc = caseSensitiveCompare
		return handler
	})
}

func match(segments []string, url string, ps *Params, ts bool) bool {
	for _, seg := range segments {
		if lu := len(url); lu == 0 {
			return false
		} else if seg[1] == ':' {
			n := len(*ps)
			*ps = (*ps)[:n+1]
			end := 1
			for end < lu && url[end] != '/' {
				end++
			}

			(*ps)[n].Key, (*ps)[n].Value = seg[2:], url[1:end]
			url = url[end:]
		} else if seg[1] == '*' {
			n := len(*ps)
			*ps = (*ps)[:n+1]
			(*ps)[n].Key, (*ps)[n].Value = seg[2:], url
			return true
		} else if lu < len(seg) {
			return false
		} else if compareFunc(url[:len(seg)], seg) {
			url = url[len(seg):]
		} else {
			return false
		}
	}
	return (!ts && url == "") || (ts && url == "/") // match trailing slash
}

// the default static segment comparison function
func caseSensitiveCompare(s1, s2 string) bool {
	return s1 == s2
}

// compareFunc is used to compare static path
// can be overriden by ComparesPathWith middleware
var compareFunc func(string, string) bool = caseSensitiveCompare

type parameters struct {
	io.ReadCloser
	params  Params
	pattern string
	pool    *sync.Pool
}

func (p *parameters) wrap(req *http.Request) {
	p.ReadCloser = req.Body
	req.Body = p
}

func (p *parameters) reset(req *http.Request) {
	if p.pool != nil { // only routes with path parameters have a pool
		p.params = p.params[0:0]
		p.pool.Put(p)
	}
	req.Body = p.ReadCloser
}

func parameterized(req *http.Request) *parameters {
	if p, ok := req.Body.(*parameters); ok {
		return p
	}
	return nil
}

var emptyParams = make(Params, 0, 0)
