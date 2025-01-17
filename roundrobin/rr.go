// Package roundrobin implements dynamic weighted round robin load balancer http handler
package roundrobin

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"

	"github.com/17media/oxy/utils"
	"github.com/gomodule/redigo/redis"
	log "github.com/sirupsen/logrus"
)

const (
	maxConnections = 10
	defaultTTL     = 3 * 24 * 60 * 60 //3 days in seconds
)

// Service service
type Service struct {
	Pool *redis.Pool
}

// NewInput input for constructor
type NewInput struct {
	RedisAddr string
	MaxConn   int64
}

var RedisSvc *Service

func init() {

	redisHost := os.Getenv("REDISHOST")
	redisPort := os.Getenv("REDISPORT")
	redisAddr := fmt.Sprintf("%s:%s", redisHost, redisPort)

	RedisSvc = NewRedis(&NewInput{
		RedisAddr: redisAddr,
		MaxConn:   maxConnections,
	},
	)

}

// New return new service
func NewRedis(input *NewInput) *Service {
	if input == nil {
		log.WithField("err", "input is required").Fatal()
	}
	redisPool := &redis.Pool{
		MaxIdle: int(input.MaxConn),
		Dial:    func() (redis.Conn, error) { return redis.Dial("tcp", input.RedisAddr) },
	}

	return &Service{
		Pool: redisPool,
	}
}

func (s *Service) getConn() (redis.Conn, error) {
	conn := s.Pool.Get()
	if err := conn.Err(); err != nil {
		return nil, err
	}
	return conn, nil
}

func (s *Service) connDo(command string, args ...interface{}) (interface{}, error) {
	conn, err := s.getConn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	return conn.Do(command, args...)
}

func (s *Service) get(key string) (val string, err error) {
	val, err = redis.String(s.connDo("GET", key))
	if err != nil {
		return "", err
	}
	return val, nil
}

func (s *Service) set(key, value string) error {
	_, err := s.connDo("SETEX", key, defaultTTL, value)
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) delete(key string) error {
	_, err := s.connDo("DEL", key)
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) expire(key string) bool {
	resp, err := s.connDo("EXPIRE", key, defaultTTL)
	if err != nil {
		return false
	}
	return resp == 1
}

// Weight is an optional functional argument that sets weight of the server
func Weight(w int) ServerOption {
	return func(s *server) error {
		if w < 0 {
			return fmt.Errorf("Weight should be >= 0")
		}
		s.weight = w
		return nil
	}
}

// ErrorHandler is a functional argument that sets error handler of the server
func ErrorHandler(h utils.ErrorHandler) LBOption {
	return func(s *RoundRobin) error {
		s.errHandler = h
		return nil
	}
}

// EnableStickySession enable sticky session
func EnableStickySession(stickySession *StickySession) LBOption {
	return func(s *RoundRobin) error {
		s.stickySession = stickySession
		return nil
	}
}

// RoundRobinRequestRewriteListener is a functional argument that sets error handler of the server
func RoundRobinRequestRewriteListener(rrl RequestRewriteListener) LBOption {
	return func(s *RoundRobin) error {
		s.requestRewriteListener = rrl
		return nil
	}
}

// RoundRobin implements dynamic weighted round robin load balancer http handler
type RoundRobin struct {
	mutex      *sync.Mutex
	next       http.Handler
	errHandler utils.ErrorHandler
	// Current index (starts from -1)
	index                  int
	servers                []*server
	currentWeight          int
	stickySession          *StickySession
	requestRewriteListener RequestRewriteListener

	log *log.Logger
}

// New created a new RoundRobin
func New(next http.Handler, opts ...LBOption) (*RoundRobin, error) {
	rr := &RoundRobin{
		next:          next,
		index:         -1,
		mutex:         &sync.Mutex{},
		servers:       []*server{},
		stickySession: nil,

		log: log.StandardLogger(),
	}
	for _, o := range opts {
		if err := o(rr); err != nil {
			return nil, err
		}
	}
	if rr.errHandler == nil {
		rr.errHandler = utils.DefaultHandler
	}
	return rr, nil
}

// RoundRobinLogger defines the logger the round robin load balancer will use.
//
// It defaults to logrus.StandardLogger(), the global logger used by logrus.
func RoundRobinLogger(l *log.Logger) LBOption {
	return func(r *RoundRobin) error {
		r.log = l
		return nil
	}
}

// Next returns the next handler
func (r *RoundRobin) Next() http.Handler {
	return r.next
}

func (r *RoundRobin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.log.Level >= log.DebugLevel {
		logEntry := r.log.WithField("Request", utils.DumpHttpRequest(req))
		logEntry.Debug("vulcand/oxy/roundrobin/rr: begin ServeHttp on request")
		defer logEntry.Debug("vulcand/oxy/roundrobin/rr: completed ServeHttp on request")
	}

	// make shallow copy of request before chaning anything to avoid side effects
	newReq := *req

	key := newReq.RequestURI
	stuck := false
	servers := r.Servers()

	if pod, err := RedisSvc.get(key); err == nil {
		// check if the pod is unhealthy or terminated
		// if it is, remove the pod from redis
		isExist := false
		for _, url := range servers {
			ok, err := r.areURLEqual(pod, url)

			if err != nil {
				log.Warnf("error parsing url: %v", err)
			}

			if ok {
				if ok = RedisSvc.expire(key); !ok {
					log.Errorf("expire redis key failed: %s", key)
					continue
				}
				newReq.URL = url
				stuck = true
				isExist = true
				break
			}
		}

		if !isExist {
			RedisSvc.delete(pod)
		}
	}

	if !stuck {
		url, err := r.NextServer()
		if err != nil {
			r.errHandler.ServeHTTP(w, req, err)
			return
		}

		newReq.URL = url

		RedisSvc.set(key, url.String())
	}

	if r.log.Level >= log.DebugLevel {
		// log which backend URL we're sending this request to
		r.log.WithFields(log.Fields{"Request": utils.DumpHttpRequest(req), "ForwardURL": newReq.URL}).Debugf("vulcand/oxy/roundrobin/rr: Forwarding this request to URL")
	}

	// Emit event to a listener if one exists
	if r.requestRewriteListener != nil {
		r.requestRewriteListener(req, &newReq)
	}

	r.next.ServeHTTP(w, &newReq)
}

// areURLEqual compare a string to a url and check if the string is the same as the url value.
func (r *RoundRobin) areURLEqual(normalized string, u *url.URL) (bool, error) {
	u1, err := url.Parse(normalized)
	if err != nil {
		return false, err
	}

	return u1.Scheme == u.Scheme && u1.Host == u.Host && u1.Path == u.Path, nil
}

// NextServer gets the next server
func (r *RoundRobin) NextServer() (*url.URL, error) {
	srv, err := r.nextServer()
	if err != nil {
		return nil, err
	}
	return utils.CopyURL(srv.url), nil
}

func (r *RoundRobin) nextServer() (*server, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if len(r.servers) == 0 {
		return nil, fmt.Errorf("no servers in the pool")
	}

	// The algo below may look messy, but is actually very simple
	// it calculates the GCD  and subtracts it on every iteration, what interleaves servers
	// and allows us not to build an iterator every time we readjust weights

	// GCD across all enabled servers
	gcd := r.weightGcd()
	// Maximum weight across all enabled servers
	max := r.maxWeight()

	for {
		r.index = (r.index + 1) % len(r.servers)
		if r.index == 0 {
			r.currentWeight = r.currentWeight - gcd
			if r.currentWeight <= 0 {
				r.currentWeight = max
				if r.currentWeight == 0 {
					return nil, fmt.Errorf("all servers have 0 weight")
				}
			}
		}
		srv := r.servers[r.index]
		if srv.weight >= r.currentWeight {
			return srv, nil
		}
	}
}

// RemoveServer remove a server
func (r *RoundRobin) RemoveServer(u *url.URL) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	e, index := r.findServerByURL(u)
	if e == nil {
		return fmt.Errorf("server not found")
	}
	r.servers = append(r.servers[:index], r.servers[index+1:]...)
	r.resetState()
	return nil
}

// Servers gets servers URL
func (r *RoundRobin) Servers() []*url.URL {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	out := make([]*url.URL, len(r.servers))
	for i, srv := range r.servers {
		out[i] = srv.url
	}
	return out
}

// ServerWeight gets the server weight
func (r *RoundRobin) ServerWeight(u *url.URL) (int, bool) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if s, _ := r.findServerByURL(u); s != nil {
		return s.weight, true
	}
	return -1, false
}

// UpsertServer In case if server is already present in the load balancer, returns error
func (r *RoundRobin) UpsertServer(u *url.URL, options ...ServerOption) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if u == nil {
		return fmt.Errorf("server URL can't be nil")
	}

	if s, _ := r.findServerByURL(u); s != nil {
		for _, o := range options {
			if err := o(s); err != nil {
				return err
			}
		}
		r.resetState()
		return nil
	}

	srv := &server{url: utils.CopyURL(u)}
	for _, o := range options {
		if err := o(srv); err != nil {
			return err
		}
	}

	if srv.weight == 0 {
		srv.weight = defaultWeight
	}

	r.servers = append(r.servers, srv)
	r.resetState()
	return nil
}

func (r *RoundRobin) resetIterator() {
	r.index = -1
	r.currentWeight = 0
}

func (r *RoundRobin) resetState() {
	r.resetIterator()
}

func (r *RoundRobin) findServerByURL(u *url.URL) (*server, int) {
	if len(r.servers) == 0 {
		return nil, -1
	}
	for i, s := range r.servers {
		if sameURL(u, s.url) {
			return s, i
		}
	}
	return nil, -1
}

func (r *RoundRobin) maxWeight() int {
	max := -1
	for _, s := range r.servers {
		if s.weight > max {
			max = s.weight
		}
	}
	return max
}

func (r *RoundRobin) weightGcd() int {
	divisor := -1
	for _, s := range r.servers {
		if divisor == -1 {
			divisor = s.weight
		} else {
			divisor = gcd(divisor, s.weight)
		}
	}
	return divisor
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// ServerOption provides various options for server, e.g. weight
type ServerOption func(*server) error

// LBOption provides options for load balancer
type LBOption func(*RoundRobin) error

// Set additional parameters for the server can be supplied when adding server
type server struct {
	url *url.URL
	// Relative weight for the enpoint to other enpoints in the load balancer
	weight int
}

var defaultWeight = 1

// SetDefaultWeight sets the default server weight
func SetDefaultWeight(weight int) error {
	if weight < 0 {
		return fmt.Errorf("default weight should be >= 0")
	}
	defaultWeight = weight
	return nil
}

func sameURL(a, b *url.URL) bool {
	return a.Path == b.Path && a.Host == b.Host && a.Scheme == b.Scheme
}

type balancerHandler interface {
	Servers() []*url.URL
	ServeHTTP(w http.ResponseWriter, req *http.Request)
	ServerWeight(u *url.URL) (int, bool)
	RemoveServer(u *url.URL) error
	UpsertServer(u *url.URL, options ...ServerOption) error
	NextServer() (*url.URL, error)
	Next() http.Handler
}
