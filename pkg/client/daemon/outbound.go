package daemon

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/pkg/errors"

	"github.com/datawire/dlib/dcontext"
	"github.com/datawire/dlib/dexec"
	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dlog"
	"github.com/datawire/telepresence2/pkg/client/daemon/dns"
	"github.com/datawire/telepresence2/pkg/client/daemon/nat"
	"github.com/datawire/telepresence2/pkg/client/daemon/proxy"
	rpc "github.com/datawire/telepresence2/pkg/rpc/daemon"
)

const (
	// worker names
	translatorWorker = "NAT"
	proxyWorker      = "PXY"
	dnsServerWorker  = "DNS"
	dnsConfigWorker  = "CFG"

	// dnsRedirPort is the port to which we redirect dns requests. It
	// should probably eventually be configurable and/or dynamically
	// chosen
	dnsRedirPort = "1233"

	// proxyRedirPort is the port to which we redirect proxied IPs. It
	// should probably eventually be configurable and/or dynamically
	// chosen.
	proxyRedirPort = "1234"
)

func dnsListeners(c context.Context, port string) (listeners []string) {
	// turns out you need to listen on localhost for nat to work
	// properly for udp, otherwise you get an "unexpected source
	// blah thingy" because the dns reply packets look like they
	// are coming from the wrong place
	listeners = append(listeners, "127.0.0.1:"+port)

	_, err := os.Stat("/.dockerenv")
	insideDocker := err == nil

	if runtime.GOOS == "linux" && !insideDocker {
		// This is the default docker bridge. We need to listen here because the nat logic we use to intercept
		// dns packets will divert the packet to the interface it originates from, which in the case of
		// containers is the docker bridge. Without this dns won't work from inside containers.
		output, err := dexec.CommandContext(c, "docker", "inspect", "bridge",
			"-f", "{{(index .IPAM.Config 0).Gateway}}").Output()
		if err != nil {
			dlog.Error(c, "not listening on docker bridge")
			return
		}
		extraIP := strings.TrimSpace(string(output))
		if extraIP != "127.0.0.1" && extraIP != "0.0.0.0" && extraIP != "" {
			listeners = append(listeners, fmt.Sprintf("%s:%s", extraIP, port))
		}
	}
	return
}

// start starts the interceptor, and only returns once the
// interceptor is successfully running in another goroutine.  It
// returns a function to call to shut down that goroutine.
//
// If dnsIP is empty, it will be detected from /etc/resolv.conf
//
// If fallbackIP is empty, it will default to Google DNS.
func start(c context.Context, dnsIP, fallbackIP string, noSearch bool) (*outbound, error) {
	ic := newOutbound("traffic-manager", dnsIP, fallbackIP, noSearch, nil)
	g := dgroup.ParentGroup(c)
	g.Go(dnsServerWorker, ic.dnsServerWorker)
	return ic, nil
}

type outbound struct {
	dnsIP      string
	fallbackIP string
	noSearch   bool
	translator *nat.Translator
	tables     map[string]*rpc.Table
	tablesLock sync.RWMutex

	domains           map[string]*rpc.Route
	domainsLock       sync.RWMutex
	setSearchPathFunc func(c context.Context, paths []string)

	search     []string
	searchLock sync.RWMutex

	overridePrimaryDNS bool

	work   chan func(context.Context) error
	cancel context.CancelFunc
}

func newOutbound(name string, dnsIP, fallbackIP string, noSearch bool, cancel context.CancelFunc) *outbound {
	ret := &outbound{
		dnsIP:      dnsIP,
		fallbackIP: fallbackIP,
		noSearch:   noSearch,
		tables:     make(map[string]*rpc.Table),
		translator: nat.NewTranslator(name),
		domains:    make(map[string]*rpc.Route),
		search:     []string{""},
		work:       make(chan func(context.Context) error),
		cancel:     cancel,
	}
	ret.tablesLock.Lock() // leave it locked until translatorWorker unlocks it
	return ret
}

func (o *outbound) runLocalServer(c context.Context) error {
	if o.dnsIP == "" {
		dat, err := ioutil.ReadFile("/etc/resolv.conf")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(dat), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "nameserver") {
				fields := strings.Fields(line)
				o.dnsIP = fields[1]
				dlog.Infof(c, "Automatically set -dns=%v", o.dnsIP)
				break
			}
		}
	}
	if o.dnsIP == "" {
		return errors.New("couldn't determine dns ip from /etc/resolv.conf")
	}

	if o.fallbackIP == "" {
		if o.dnsIP == "8.8.8.8" {
			o.fallbackIP = "8.8.4.4"
		} else {
			o.fallbackIP = "8.8.8.8"
		}
		dlog.Infof(c, "Automatically set -fallback=%v", o.fallbackIP)
	}
	if o.fallbackIP == o.dnsIP {
		return errors.New("if your fallbackIP and your dnsIP are the same, you will have a dns loop")
	}

	o.setSearchPathFunc = func(c context.Context, paths []string) {
		o.searchLock.Lock()
		o.search = paths
		o.searchLock.Unlock()
	}

	o.overridePrimaryDNS = true
	dgroup.ParentGroup(c).Go(proxyWorker, o.proxyWorker)

	srv := dns.NewServer(c, dnsListeners(c, dnsRedirPort), o.fallbackIP+":53", func(domain string) string {
		if r := o.resolve(domain); r != nil {
			return r.Ip
		}
		return ""
	})
	dlog.Debug(c, "Starting server")
	initDone := &sync.WaitGroup{}
	initDone.Add(1)
	err := srv.Run(c, initDone)
	dlog.Debug(c, "Server done")
	return err
}

func (o *outbound) proxyWorker(c context.Context) error {
	// hmm, we may not actually need to get the original
	// destination, we could just forward each ip to a unique port
	// and either listen on that port or run port-forward
	pr, err := proxy.NewProxy(c, ":"+proxyRedirPort, o.destination)
	if err != nil {
		return errors.Wrap(err, "Proxy")
	}
	dgroup.ParentGroup(c).Go(translatorWorker, o.translatorWorker)
	dlog.Debug(c, "Starting server")
	pr.Run(c, 10000)
	dlog.Debug(c, "Server done")
	return nil
}

func (o *outbound) dnsConfigWorker(c context.Context) error {
	dlog.Debug(c, "Starting server")
	bootstrap := rpc.Table{Name: "bootstrap", Routes: []*rpc.Route{{
		Ip:     o.dnsIP,
		Target: dnsRedirPort,
		Proto:  "udp",
	}}}
	o.update(&bootstrap)
	dns.Flush()

	if o.noSearch {
		dlog.Debug(c, "Server done")
		return nil
	}

	restore, err := dns.OverrideSearchDomains(c, ".")
	if err != nil {
		return err
	}
	<-c.Done()
	restore()
	dns.Flush()
	dlog.Debug(c, "Server done")
	return nil
}

func (o *outbound) translatorWorker(c context.Context) (err error) {
	defer func() {
		o.tablesLock.Lock()
		if err2 := o.translator.Disable(c); err2 != nil {
			if err == nil {
				err = err2
			} else {
				dlog.Error(c, err2)
			}
		}
		if err != nil {
			dlog.Errorf(c, "Server exited with error %v", err)
		} else {
			dlog.Debug(c, "Server done")
		}
		// leave it locked
	}()

	dlog.Debug(c, "Enabling")
	err = o.translator.Enable(c)
	if err != nil {
		return err
	}
	o.tablesLock.Unlock()

	if o.overridePrimaryDNS {
		dgroup.ParentGroup(c).Go(dnsConfigWorker, o.dnsConfigWorker)
	}

	dlog.Debug(c, "Starting server")
	for {
		select {
		case <-c.Done():
			c = dcontext.HardContext(c)
			dlog.Debug(c, "context cancelled, shutting down")
			go func() {
				// drain work queue (unlock and toss remaining work)
				for range o.work {
				}
			}()
			close(o.work)
			return nil
		case f := <-o.work:
			if f == nil {
				return
			}
			if err = f(c); err != nil {
				dlog.Error(c, err)
			}
		}
	}
}

// resolve looks up the given query in the (FIXME: somewhere), trying
// all the suffixes in the search path, and returns a Route on success
// or nil on failure. This implementation does not count the number of
// dots in the query.
func (o *outbound) resolve(query string) *rpc.Route {
	if !strings.HasSuffix(query, ".") {
		query += "."
	}

	var route *rpc.Route
	o.searchLock.RLock()
	o.domainsLock.RLock()
	for _, suffix := range o.search {
		name := query + suffix
		if route = o.domains[strings.ToLower(name)]; route != nil {
			break
		}
	}
	o.searchLock.RUnlock()
	o.domainsLock.RUnlock()
	return route
}

func (o *outbound) destination(conn *net.TCPConn) (string, error) {
	_, host, err := o.translator.GetOriginalDst(conn)
	return host, err
}

func (o *outbound) update(table *rpc.Table) {
	o.work <- func(c context.Context) error {
		return o.doUpdate(c, table)
	}
}

func routesEqual(a, b *rpc.Route) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Name == b.Name && a.Action == b.Action && a.Ip == b.Ip && a.Port == b.Port && a.Target == b.Target
}

func domain(r *rpc.Route) string {
	return strings.ToLower(r.Name + ".")
}

func (o *outbound) doUpdate(c context.Context, table *rpc.Table) error {
	// Make a copy of the current table
	o.tablesLock.RLock()
	oldTable, ok := o.tables[table.Name]
	oldRoutes := make(map[string]*rpc.Route)
	if ok {
		for _, route := range oldTable.Routes {
			oldRoutes[route.Name] = route
		}
	}
	o.tablesLock.RUnlock()

	// Operate on the copy of the current table and the new table
	for _, newRoute := range table.Routes {
		oldRoute, oldRouteOk := oldRoutes[newRoute.Name]
		// A nil Route (when oldRouteOk != true) will compare
		// inequal to any valid new Route.
		if !routesEqual(newRoute, oldRoute) {
			// We're updating a route. Make sure DNS waits until the new answer
			// is ready, i.e. don't serve the old answer.
			o.domainsLock.Lock()

			// delete the old version
			if oldRouteOk {
				switch newRoute.Proto {
				case "tcp":
					if err := o.translator.ClearTCP(c, oldRoute.Ip, oldRoute.Port); err != nil {
						dlog.Errorf(c, "clear tpc: %v", err)
					}
				case "udp":
					if err := o.translator.ClearUDP(c, oldRoute.Ip, oldRoute.Port); err != nil {
						dlog.Errorf(c, "clear udp: %v", err)
					}
				default:
					dlog.Warnf(c, "unrecognized protocol: %v", newRoute)
				}
			}
			// and add the new version
			if newRoute.Target != "" {
				switch newRoute.Proto {
				case "tcp":
					if err := o.translator.ForwardTCP(c, newRoute.Ip, newRoute.Port, newRoute.Target); err != nil {
						dlog.Errorf(c, "forward tcp: %v", err)
					}
				case "udp":
					if err := o.translator.ForwardUDP(c, newRoute.Ip, newRoute.Port, newRoute.Target); err != nil {
						dlog.Errorf(c, "forward udp: %v", err)
					}
				default:
					dlog.Warnf(c, "unrecognized protocol: %v", newRoute)
				}
			}

			if newRoute.Name != "" {
				domain := domain(newRoute)
				dlog.Debugf(c, "STORE %v->%v", domain, newRoute)
				o.domains[domain] = newRoute
			}

			o.domainsLock.Unlock()
		}

		// remove the route from our map of old routes so we
		// don't end up deleting it below
		delete(oldRoutes, newRoute.Name)
	}

	// Clear out stale routes and DNS names
	o.domainsLock.Lock()
	for _, route := range oldRoutes {
		domain := domain(route)
		dlog.Debugf(c, "CLEAR %v->%v", domain, route)
		delete(o.domains, domain)

		switch route.Proto {
		case "tcp":
			if err := o.translator.ClearTCP(c, route.Ip, route.Port); err != nil {
				dlog.Errorf(c, "clear tpc: %v", err)
			}
		case "udp":
			if err := o.translator.ClearUDP(c, route.Ip, route.Port); err != nil {
				dlog.Errorf(c, "clear udp: %v", err)
			}
		default:
			dlog.Warnf(c, "unrecognized protocol: %v", route)
		}
	}
	o.domainsLock.Unlock()

	// Update the externally-visible table
	o.tablesLock.Lock()
	if table.Routes == nil || len(table.Routes) == 0 {
		delete(o.tables, table.Name)
	} else {
		o.tables[table.Name] = table
	}
	o.tablesLock.Unlock()

	return nil
}

// SetSearchPath updates the DNS search path used by the resolver
func (o *outbound) setSearchPath(c context.Context, paths []string) {
	o.searchLock.Lock()
	defer o.searchLock.Unlock()
	o.setSearchPathFunc(c, paths)
}

func (o *outbound) shutdown() {
	o.cancel()
}
