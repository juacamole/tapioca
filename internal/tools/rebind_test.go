package tools

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A DNS server that answers the same name with a different address every time,
// which is all a rebinding attack is. The first answer is what a check on the
// host name sees; every answer after it is what the connection actually gets.
type flipResolver struct {
	first, rest net.IP
	aQueries    atomic.Int32
}

// serve starts the server on loopback and returns a resolver that talks to it.
func (f *flipResolver) serve(t *testing.T) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen for DNS: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply := f.answer(buf[:n]); reply != nil {
				_, _ = pc.WriteTo(reply, addr)
			}
		}
	}()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", pc.LocalAddr().String())
		},
	}
}

// answer builds a reply to one query: an A record for A queries, an empty
// answer for everything else, so only the address family under test matters.
func (f *flipResolver) answer(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	i := 12
	for i < len(q) && q[i] != 0 { // walk the labels of QNAME
		i += int(q[i]) + 1
	}
	i++ // the root label
	if i+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[i:])
	end := i + 4
	reply := append([]byte(nil), q[:end]...)
	reply[2], reply[3] = 0x81, 0x80 // response, recursion available
	if qtype != 1 {                 // not A: NOERROR with no records
		binary.BigEndian.PutUint16(reply[6:], 0)
		return reply
	}
	ip := f.rest
	if f.aQueries.Add(1) == 1 {
		ip = f.first
	}
	binary.BigEndian.PutUint16(reply[6:], 1) // one answer
	reply = append(reply, 0xc0, 0x0c)        // name: pointer to the question
	reply = append(reply, 0, 1, 0, 1)        // type A, class IN
	reply = append(reply, 0, 0, 0, 0)        // TTL 0: answer again next time
	reply = append(reply, 0, 4)
	return append(reply, ip.To4()...)
}

// webFetch screens the host name it was given by resolving it, and then hands
// the name to the transport, which resolves it a second time. Nothing binds
// the two answers together: a name whose record changes between them — a zero
// TTL, or a server that simply answers differently — is screened as one
// address and connected to as another, and 169.254.169.254 is what that is
// worth. The refusal has to be made where the address is known, which is the
// moment before the socket connects.
func TestFetchRefusesALinkLocalAddressItWasReboundTo(t *testing.T) {
	f := &flipResolver{
		first: net.ParseIP("203.0.113.1"),     // TEST-NET-3: screens clean
		rest:  net.ParseIP("169.254.169.254"), // where the connection goes
	}
	old := net.DefaultResolver
	net.DefaultResolver = f.serve(t)
	t.Cleanup(func() { net.DefaultResolver = old })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	e := NewExecutor(t.TempDir(), ModeBypass)
	raw, _ := json.Marshal(map[string]string{"url": "http://rebind.test./"})
	out, isErr, err := e.webFetch(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	// The control: the screening lookup happened, saw the clean answer and let
	// the fetch through, and the transport then resolved the name a second
	// time. One query would mean the name check caught it and the rebinding
	// never happened, so nothing below would be about the connection.
	if n := f.aQueries.Load(); n < 2 {
		t.Skipf("the name was resolved %d time(s); this build does not resolve twice", n)
	}
	if !isErr {
		t.Fatalf("the fetch succeeded: %q", out)
	}
	if !strings.Contains(out, "refusing to connect to the link-local address") {
		t.Errorf("the fetch was attempted against the address it was rebound to; "+
			"it failed only because nothing answered there: %q", out)
	}
}

// The same check must not refuse the addresses a fetch is allowed to reach: a
// dev server on localhost and a machine on the LAN are the ordinary reasons to
// fetch a private address at all.
func TestDialCheckAllowsLoopbackAndPrivateAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "192.168.1.10:80", "10.0.0.1:443", "93.184.216.34:80"} {
		if err := refuseLinkLocal("tcp", addr, nil); err != nil {
			t.Errorf("%s refused: %v", addr, err)
		}
	}
	for _, addr := range []string{"169.254.169.254:80", "[fe80::1%eth0]:80"} {
		if err := refuseLinkLocal("tcp", addr, nil); err == nil {
			t.Errorf("%s was not refused", addr)
		}
	}
}
