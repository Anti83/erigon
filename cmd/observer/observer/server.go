package observer

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"github.com/ledgerwatch/erigon/cmd/utils"
	"github.com/ledgerwatch/erigon/common/debug"
	"github.com/ledgerwatch/erigon/p2p"
	"github.com/ledgerwatch/erigon/p2p/discover"
	"github.com/ledgerwatch/erigon/p2p/enode"
	"github.com/ledgerwatch/erigon/p2p/nat"
	"github.com/ledgerwatch/erigon/p2p/netutil"
	"github.com/ledgerwatch/log/v3"
	"net"
	"path/filepath"
)

type Server struct {
	localNode *enode.LocalNode

	listenAddr   string
	natInterface nat.Interface
	discConfig   discover.Config

	discV4 *discover.UDPv4

	log log.Logger
}

func NewServer(flags CommandFlags) (*Server, error) {
	nodeDBPath := filepath.Join(flags.DataDir, "nodes", "eth66")

	nodeKeyConfig := p2p.NodeKeyConfig{}
	privateKey, err := nodeKeyConfig.LoadOrParseOrGenerateAndSave(flags.NodeKeyFile, flags.NodeKeyHex, flags.DataDir)
	if err != nil {
		return nil, err
	}

	localNode, err := makeLocalNode(nodeDBPath, privateKey)
	if err != nil {
		return nil, err
	}

	listenAddr := fmt.Sprintf(":%d", flags.ListenPort)

	natInterface, err := nat.Parse(flags.NATDesc)
	if err != nil {
		return nil, fmt.Errorf("NAT parse error: %w", err)
	}

	var netRestrictList *netutil.Netlist
	if flags.NetRestrict != "" {
		netRestrictList, err = netutil.ParseNetlist(flags.NetRestrict)
		if err != nil {
			return nil, fmt.Errorf("net restrict parse error: %w", err)
		}
	}

	bootnodes, err := utils.GetBootnodesFromFlags(flags.Bootnodes, flags.Chain)
	if err != nil {
		return nil, fmt.Errorf("bootnodes parse error: %w", err)
	}

	logger := log.New()

	discConfig := discover.Config{
		PrivateKey:  privateKey,
		NetRestrict: netRestrictList,
		Bootnodes:   bootnodes,
		Log:         logger.New(),
	}

	instance := Server{
		localNode:    localNode,
		listenAddr:   listenAddr,
		natInterface: natInterface,
		discConfig:   discConfig,
		log:          logger,
	}
	return &instance, nil
}

func makeLocalNode(nodeDBPath string, privateKey *ecdsa.PrivateKey) (*enode.LocalNode, error) {
	db, err := enode.OpenDB(nodeDBPath)
	if err != nil {
		return nil, err
	}
	localNode := enode.NewLocalNode(db, privateKey)
	localNode.SetFallbackIP(net.IP{127, 0, 0, 1})
	return localNode, nil
}

func (server *Server) mapNATPort(ctx context.Context, realAddr *net.UDPAddr) {
	if server.natInterface == nil {
		return
	}
	if realAddr.IP.IsLoopback() {
		return
	}
	if !server.natInterface.SupportsMapping() {
		return
	}

	go func() {
		defer debug.LogPanic()
		nat.Map(server.natInterface, ctx.Done(), "udp", realAddr.Port, realAddr.Port, "ethereum discovery")
	}()
}

func (server *Server) detectNATExternalIP() (net.IP, error) {
	if server.natInterface == nil {
		return nil, errors.New("no NAT flag configured")
	}
	if _, hasExtIP := server.natInterface.(nat.ExtIP); !hasExtIP {
		server.log.Info("Detecting external IP...")
	}
	ip, err := server.natInterface.ExternalIP()
	if err != nil {
		return nil, fmt.Errorf("NAT ExternalIP error: %w", err)
	}
	server.log.Debug("External IP detected", "ip", ip)
	return ip, nil
}

func (server *Server) Listen(ctx context.Context) error {
	discV4, err := server.listenDiscovery(ctx)
	if err != nil {
		return err
	}

	server.discV4 = discV4

	<-ctx.Done()
	return nil
}

func (server *Server) listenDiscovery(ctx context.Context) (*discover.UDPv4, error) {
	if server.natInterface != nil {
		ip, err := server.detectNATExternalIP()
		if err != nil {
			return nil, err
		}
		server.localNode.SetStaticIP(ip)
	}

	addr, err := net.ResolveUDPAddr("udp", server.listenAddr)
	if err != nil {
		return nil, fmt.Errorf("ResolveUDPAddr error: %w", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("ListenUDP error: %w", err)
	}

	realAddr := conn.LocalAddr().(*net.UDPAddr)
	server.localNode.SetFallbackUDP(realAddr.Port)

	if server.natInterface != nil {
		server.mapNATPort(ctx, realAddr)
	}

	server.log.Debug("UDP listener up", "addr", realAddr)

	return discover.ListenV4(ctx, conn, server.localNode, server.discConfig)
}