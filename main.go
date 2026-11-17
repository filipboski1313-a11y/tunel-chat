package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/routing"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	routedhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	mh "github.com/multiformats/go-multihash"
)

const chatProtocol = protocol.ID("/tunel-chat/1.0.0")

const (
	colorReset  = "\033[0m"
	colorCyan   = "\033[36m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
	colorGray   = "\033[90m"
	colorBold   = "\033[1m"
)

func peerCID(pid peer.ID) cid.Cid {
	hash, _ := mh.Sum([]byte("tunel-chat:"+pid.String()), mh.SHA2_256, -1)
	return cid.NewCidV1(cid.Raw, hash)
}

type mdnsNotifee struct {
	h host.Host
}

func (n *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == n.h.ID() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = n.h.Connect(ctx, pi)
}

func main() {
	// Silence internal libp2p logs to keep the terminal clean
	logging.SetAllLoggers(logging.LevelFatal)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl+C / SIGTERM cleanly
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Printf("\n%s[Closing tunnel and wiping memory...]%s\n", colorYellow, colorReset)
		cancel()
		os.Exit(0)
	}()

	fmt.Printf("%s=== TUNEL-CHAT (P2P / E2E / ZERO STORAGE) ===%s\n", colorBold+colorCyan, colorReset)
	fmt.Printf("%sInitializing encrypted node...%s\n", colorGray, colorReset)

	var kadDHT *dht.IpfsDHT
	peerSource := func(ctx context.Context, numPeers int) <-chan peer.AddrInfo {
		ch := make(chan peer.AddrInfo, numPeers)
		go func() {
			defer close(ch)
			for _, addr := range dht.DefaultBootstrapPeers {
				pi, err := peer.AddrInfoFromP2pAddr(addr)
				if err != nil {
					continue
				}
				select {
				case ch <- *pi:
				case <-ctx.Done():
					return
				}
			}
		}()
		return ch
	}

	rawHost, err := libp2p.New(
		libp2p.ListenAddrStrings(
			"/ip4/0.0.0.0/udp/0/quic-v1",
			"/ip4/0.0.0.0/tcp/0",
		),
		libp2p.NATPortMap(),
		libp2p.EnableHolePunching(),
		libp2p.EnableAutoRelayWithPeerSource(peerSource, autorelay.WithMinInterval(15*time.Second)),
		libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
			var err error
			kadDHT, err = dht.New(h, dht.Mode(dht.ModeClient))
			return kadDHT, err
		}),
	)
	if err != nil {
		fmt.Printf("%sFailed to start node: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}
	defer rawHost.Close()

	// Wrap host in RoutedHost for automatic peer discovery via DHT
	h := routedhost.Wrap(rawHost, kadDHT)

	// Local mDNS discovery for computers on the same Wi-Fi/LAN
	if mdnsSvc := mdns.NewMdnsService(h, "tunel-chat", &mdnsNotifee{h: h}); mdnsSvc != nil {
		if err := mdnsSvc.Start(); err == nil {
			defer mdnsSvc.Close()
		}
	}

	// Channel for incoming chat streams
	incomingCh := make(chan network.Stream, 1)
	var activeMu sync.Mutex
	var inChat bool

	h.SetStreamHandler(chatProtocol, func(s network.Stream) {
		activeMu.Lock()
		busy := inChat
		if !busy {
			inChat = true
		}
		activeMu.Unlock()

		if busy {
			_ = s.Reset()
			return
		}

		select {
		case incomingCh <- s:
		default:
			_ = s.Reset()
		}
	})

	// Bootstrap global DHT in the background
	dhtReady := make(chan struct{})
	go func() {
		defer close(dhtReady)
		var wg sync.WaitGroup
		for _, addr := range dht.DefaultBootstrapPeers {
			pi, err := peer.AddrInfoFromP2pAddr(addr)
			if err != nil {
				continue
			}
			wg.Add(1)
			go func(pi peer.AddrInfo) {
				defer wg.Done()
				cctx, ccancel := context.WithTimeout(ctx, 8*time.Second)
				defer ccancel()
				_ = h.Connect(cctx, pi)
			}(*pi)
		}
		wg.Wait()
		if kadDHT != nil {
			_ = kadDHT.Bootstrap(ctx)
			go func() {
				myCID := peerCID(h.ID())
				for {
					pctx, pcancel := context.WithTimeout(ctx, 20*time.Second)
					_ = kadDHT.Provide(pctx, myCID, true)
					pcancel()
					select {
					case <-ctx.Done():
						return
					case <-time.After(5 * time.Minute):
					}
				}
			}()
		}
	}()

	fmt.Printf("\n%sYour ID:%s %s%s%s\n", colorBold, colorReset, colorGreen+colorBold, h.ID().String(), colorReset)
	fmt.Printf("%s(Ephemeral ID — vanishes without a trace when you exit)%s\n\n", colorGray, colorReset)

	// Wait briefly (up to 3s) for initial network readiness
	select {
	case <-dhtReady:
		fmt.Printf("%s[Network ready]%s\n", colorGreen, colorReset)
	case <-time.After(3 * time.Second):
		fmt.Printf("%s[Connecting to network in background — you can enter an ID or wait]%s\n", colorYellow, colorReset)
	}

	// Single stdin reader for the lifetime of the program
	stdinCh := make(chan string)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(stdinCh)
				return
			}
			stdinCh <- strings.TrimRight(line, "\r\n")
		}
	}()

	for {
		activeMu.Lock()
		inChat = false
		activeMu.Unlock()

		fmt.Printf("\n%sEnter peer ID (or wait for someone to connect): %s", colorCyan, colorReset)

		var stream network.Stream
		select {
		case <-ctx.Done():
			return
		case s := <-incomingCh:
			stream = s
			fmt.Printf("\n%s[Incoming encrypted tunnel from: %s]%s\n", colorGreen, s.Conn().RemotePeer().String(), colorReset)
		case input, ok := <-stdinCh:
			if !ok {
				return
			}
			target := strings.TrimSpace(input)
			if target == "" {
				continue
			}
			if target == "/quit" || target == "exit" {
				return
			}

			pid, err := peer.Decode(target)
			if err != nil {
				fmt.Printf("%sInvalid ID: %v%s\n", colorRed, err, colorReset)
				continue
			}
			if pid == h.ID() {
				fmt.Printf("%sYou cannot connect to yourself.%s\n", colorYellow, colorReset)
				continue
			}

			fmt.Printf("%sSearching and establishing direct tunnel to %s...%s\n", colorGray, pid.String(), colorReset)
			dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
			if len(h.Peerstore().Addrs(pid)) == 0 && kadDHT != nil {
				provCh := kadDHT.FindProvidersAsync(dialCtx, peerCID(pid), 5)
				for pi := range provCh {
					if pi.ID == pid && len(pi.Addrs) > 0 {
						_ = h.Connect(dialCtx, pi)
						break
					}
				}
			}
			s, err := h.NewStream(dialCtx, pid, chatProtocol)
			dialCancel()
			if err != nil {
				fmt.Printf("%sConnection failed: %v%s\n", colorRed, err, colorReset)
				continue
			}

			activeMu.Lock()
			inChat = true
			activeMu.Unlock()
			stream = s
			fmt.Printf("%s[Connected! Encrypted tunnel active]%s\n", colorGreen, colorReset)
		}

		runChatSession(ctx, stream, stdinCh)
		_ = stream.Close()
		fmt.Printf("\n%s[Disconnected. Session data wiped.]%s\n", colorYellow, colorReset)
	}
}

func runChatSession(ctx context.Context, s network.Stream, stdinCh <-chan string) {
	remoteAddr := s.Conn().RemoteMultiaddr().String()
	connType := "direct P2P"
	if strings.Contains(remoteAddr, "/p2p-circuit") {
		connType = "circuit relay (E2E encrypted)"
	}
	fmt.Printf("%sConnection mode: %s | Type /quit to disconnect%s\n", colorGray, connType, colorReset)
	fmt.Println(strings.Repeat("-", 55))

	done := make(chan struct{})
	var once sync.Once
	closeDone := func() { once.Do(func() { close(done) }) }

	// Receive messages from the tunnel
	go func() {
		defer closeDone()
		reader := bufio.NewReader(s)
		for {
			msg, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					fmt.Printf("\n%s[Connection lost]%s\n", colorRed, colorReset)
				}
				return
			}
			msg = strings.TrimRight(msg, "\r\n")
			fmt.Printf("\r\033[K%sPeer:%s %s\n%sYou:%s ", colorCyan+colorBold, colorReset, msg, colorGreen+colorBold, colorReset)
		}
	}()

	fmt.Printf("%sYou:%s ", colorGreen+colorBold, colorReset)
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case line, ok := <-stdinCh:
			if !ok {
				return
			}
			if strings.TrimSpace(line) == "/quit" {
				return
			}
			if _, err := fmt.Fprintf(s, "%s\n", line); err != nil {
				return
			}
			fmt.Printf("%sYou:%s ", colorGreen+colorBold, colorReset)
		}
	}
}
