package tunnel

import (
	"context"
	"errors"
	"github.com/FlowerWrong/tun2socks/util"
	"log"
	"net"
	"sync"

	"github.com/FlowerWrong/netstack/tcpip"
	"github.com/FlowerWrong/netstack/tcpip/stack"
	"github.com/FlowerWrong/netstack/tcpip/transport/udp"
	"github.com/FlowerWrong/tun2socks/dns"
	"github.com/FlowerWrong/tun2socks/tun2socks"
	"github.com/yinghuocho/gosocks"
)

// Udp tunnel
type UdpTunnel struct {
	localEndpoint        stack.TransportEndpointID
	socks5TcpConn        *gosocks.SocksConn
	socks5UdpListen      *net.UDPConn
	RemotePackets        chan []byte // write to local
	LocalPackets         chan []byte // write to remote, socks5
	ctx                  context.Context
	ctxCancel            context.CancelFunc
	localAddr            tcpip.FullAddress
	cmdUDPAssociateReply *gosocks.SocksReply
	closeOne             sync.Once
	status               TunnelStatus // to avoid panic: send on closed channel
	rwMutex              sync.RWMutex
	app                  *tun2socks.App
}

// Create a udp tunnel
func NewUdpTunnel(endpoint stack.TransportEndpointID, localAddr tcpip.FullAddress, app *tun2socks.App) (*UdpTunnel, error) {
	localTcpSocks5Dialer := &gosocks.SocksDialer{
		Auth:    &gosocks.AnonymousClientAuthenticator{},
		Timeout: DefaultConnectDuration,
	}

	remoteHost := endpoint.LocalAddress.To4().String()
	proxy := ""
	if app.FakeDns != nil {
		ip := net.ParseIP(remoteHost)
		record := app.FakeDns.DnsTablePtr.GetByIP(ip)
		if record != nil {
			proxy = app.Cfg.GetProxy(record.Proxy)
		}
	}

	if proxy == "" {
		proxy, _ = app.Cfg.UdpProxy()
	}

	socks5TcpConn, err := localTcpSocks5Dialer.Dial(proxy)
	if err != nil {
		log.Println("Fail to connect SOCKS proxy ", err)
		return nil, err
	}

	udpSocks5Addr := socks5TcpConn.LocalAddr().(*net.TCPAddr)
	udpSocks5Listen, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   udpSocks5Addr.IP,
		Port: 0,
		Zone: udpSocks5Addr.Zone,
	})
	if err != nil {
		log.Println("ListenUDP falied", err)
		socks5TcpConn.Close()
		return nil, err
	}
	udpSocks5Listen.SetDeadline(WithoutTimeout)

	_, err = gosocks.WriteSocksRequest(socks5TcpConn, &gosocks.SocksRequest{
		Cmd:      gosocks.SocksCmdUDPAssociate,
		HostType: gosocks.SocksIPv4Host,
		DstHost:  "0.0.0.0",
		DstPort:  0,
	})
	if err != nil {
		// FIXME i/o timeout
		log.Println("WriteSocksRequest failed", err)
		socks5TcpConn.Close()
		udpSocks5Listen.Close()
		return nil, err
	}

	cmdUDPAssociateReply, err := gosocks.ReadSocksReply(socks5TcpConn)
	if err != nil {
		log.Println("ReadSocksReply failed", err)
		socks5TcpConn.Close()
		udpSocks5Listen.Close()
		return nil, err
	}
	if cmdUDPAssociateReply.Rep != gosocks.SocksSucceeded {
		log.Printf("socks connect request fail, retcode: %d", cmdUDPAssociateReply.Rep)
		socks5TcpConn.Close()
		udpSocks5Listen.Close()
		return nil, err
	}
	// A zero value for t means I/O operations will not time out.
	socks5TcpConn.SetDeadline(WithoutTimeout)

	return &UdpTunnel{
		localEndpoint:        endpoint,
		socks5TcpConn:        socks5TcpConn,
		socks5UdpListen:      udpSocks5Listen,
		RemotePackets:        make(chan []byte, PktChannelSize),
		LocalPackets:         make(chan []byte, PktChannelSize),
		localAddr:            localAddr,
		app:                  app,
		cmdUDPAssociateReply: cmdUDPAssociateReply,
	}, nil
}

// Set udp tunnel status with rwMutex
func (udpTunnel *UdpTunnel) SetStatus(s TunnelStatus) {
	udpTunnel.rwMutex.Lock()
	udpTunnel.status = s
	udpTunnel.rwMutex.Unlock()
}

// Get udp tunnel status with rwMutex
func (udpTunnel *UdpTunnel) Status() TunnelStatus {
	udpTunnel.rwMutex.Lock()
	s := udpTunnel.status
	udpTunnel.rwMutex.Unlock()
	return s
}

func (udpTunnel *UdpTunnel) Run() {
	udpTunnel.ctx, udpTunnel.ctxCancel = context.WithCancel(context.Background())
	go udpTunnel.writeToLocal()
	go udpTunnel.readFromRemote()
	go udpTunnel.writeToRemote()
	udpTunnel.SetStatus(StatusProxying)
}

// Write udp packet to upstream
func (udpTunnel *UdpTunnel) writeToRemote() {
writeToRemote:
	for {
		select {
		case <-udpTunnel.ctx.Done():
			break writeToRemote
		case chunk := <-udpTunnel.LocalPackets:
			remoteHost := udpTunnel.localEndpoint.LocalAddress.To4().String()
			remotePort := udpTunnel.localEndpoint.LocalPort

			var hostType byte = gosocks.SocksIPv4Host
			if udpTunnel.app.FakeDns != nil {
				ip := net.ParseIP(remoteHost)
				record := udpTunnel.app.FakeDns.DnsTablePtr.GetByIP(ip)
				if record != nil {
					remoteHost = record.Hostname
					hostType = gosocks.SocksDomainHost
				}
			}

			req := &gosocks.UDPRequest{
				Frag:     0,
				HostType: hostType,
				DstHost:  remoteHost,
				DstPort:  remotePort,
				Data:     chunk,
			}
			_, err := udpTunnel.socks5UdpListen.WriteTo(gosocks.PackUDPRequest(req), gosocks.SocksAddrToNetAddr("udp", udpTunnel.cmdUDPAssociateReply.BndHost, udpTunnel.cmdUDPAssociateReply.BndPort).(*net.UDPAddr))
			if err != nil {
				if !util.IsEOF(err) {
					log.Println("WriteTo UDP tunnel failed", err)
				}
				udpTunnel.Close(err)
				break writeToRemote
			}
		}
	}
}

// Read udp packet from upstream
func (udpTunnel *UdpTunnel) readFromRemote() {
readFromRemote:
	for {
		select {
		case <-udpTunnel.ctx.Done():
			break readFromRemote
		default:
			var udpSocks5Buf [PktChannelSize]byte
			n, _, err := udpTunnel.socks5UdpListen.ReadFromUDP(udpSocks5Buf[0:])
			if n > 0 {
				udpReq, err := gosocks.ParseUDPRequest(udpSocks5Buf[0:n])
				if err != nil {
					log.Println("Parse UDP reply data frailed", err)
					udpTunnel.Close(err)
					break readFromRemote
				}
				if udpTunnel.status != StatusClosed {
					udpTunnel.RemotePackets <- udpReq.Data
				}
			}
			if err != nil {
				if !util.IsEOF(err) {
					log.Println("ReadFromUDP tunnel failed", err)
				}
				udpTunnel.Close(err)
				break readFromRemote
			}
		}
	}
}

// Write upstream udp packet to local
func (udpTunnel *UdpTunnel) writeToLocal() {
writeToLocal:
	for {
		select {
		case <-udpTunnel.ctx.Done():
			break writeToLocal
		case chunk := <-udpTunnel.RemotePackets:
			remoteHost := udpTunnel.localEndpoint.LocalAddress.To4().String()
			remotePort := udpTunnel.localEndpoint.LocalPort
			pkt := util.CreateDNSResponse(net.ParseIP(remoteHost), remotePort, net.ParseIP(udpTunnel.localAddr.Addr.To4().String()), udpTunnel.localAddr.Port, chunk)
			if pkt == nil {
				udpTunnel.Close(errors.New("pack ip packet return nil"))
				break writeToLocal
			}
			_, err := udpTunnel.app.Ifce.Write(pkt)
			if err != nil {
				log.Println("Write to tun failed", err)
			} else {
				// cache dns packet
				if udpTunnel.app.Cfg.Dns.DnsMode == "udp_relay_via_socks5" {
					if dns.DNSCache != nil {
						dns.DNSCache.Store(chunk)
					}
				}
			}
			if err != nil {
				log.Println("Write udp package to tun failed", err)
				udpTunnel.Close(err)
			} else {
				udpTunnel.Close(errors.New("OK"))
			}
			break writeToLocal
		}
	}
}

// Close this udp tunnel
func (udpTunnel *UdpTunnel) Close(reason error) {
	udpTunnel.closeOne.Do(func() {
		udpTunnel.ctxCancel()
		udpTunnel.SetStatus(StatusClosed)
		udpTunnel.socks5TcpConn.Close()
		udpTunnel.socks5UdpListen.Close()
		udp.UDPNatList.DelUDPNat(udpTunnel.localAddr.Port)
		close(udpTunnel.LocalPackets)
		close(udpTunnel.RemotePackets)
	})
}
