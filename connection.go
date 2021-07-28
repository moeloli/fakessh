package main

import (
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/crypto/ssh"
)

type connContext struct {
	ssh.ConnMetadata
	cfg            *config
	noMoreSessions bool
}

type channelContext struct {
	connContext
	channelID int
}

var channelHandlers = map[string]func(newChannel ssh.NewChannel, context channelContext) error{
	"session":      handleSessionChannel,
	"direct-tcpip": handleDirectTCPIPChannel,
}

var (
	tcpConnectionsMetric = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tcp_connections_total",
		Help: "Total number of TCP connections",
	})
	activeTCPConnectionsMetric = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "active_tcp_connections",
		Help: "Number of active TCP connections",
	})
	sshConnectionsMetric = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ssh_connections_total",
		Help: "Total number of SSH connections",
	})
	activeSSHConnectionsMetric = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "active_ssh_connections",
		Help: "Number of active SSH connections",
	})
)

func handleConnection(conn net.Conn, cfg *config) {
	tcpConnectionsMetric.Inc()
	activeTCPConnectionsMetric.Inc()
	defer activeTCPConnectionsMetric.Dec()
	serverConn, newChannels, requests, err := ssh.NewServerConn(conn, cfg.sshConfig)
	if err != nil {
		warningLogger.Printf("Failed to establish SSH connection: %v", err)
		conn.Close()
		return
	}
	sshConnectionsMetric.Inc()
	activeSSHConnectionsMetric.Inc()
	defer activeSSHConnectionsMetric.Dec()
	var channels sync.WaitGroup
	context := connContext{ConnMetadata: serverConn, cfg: cfg}
	defer func() {
		serverConn.Close()
		channels.Wait()
		context.logEvent(connectionCloseLog{})
	}()

	context.logEvent(connectionLog{
		ClientVersion: string(serverConn.ClientVersion()),
	})

	if _, _, err := serverConn.SendRequest("hostkeys-00@openssh.com", false, createHostkeysRequestPayload(cfg.parsedHostKeys)); err != nil {
		warningLogger.Printf("Failed to send hostkeys-00@openssh.com request: %v", err)
		return
	}

	channelID := 0
	for requests != nil || newChannels != nil {
		select {
		case request, ok := <-requests:
			if !ok {
				requests = nil
				continue
			}
			if err := handleGlobalRequest(request, &context); err != nil {
				warningLogger.Printf("Failed to handle global request: %v", err)
				requests = nil
				continue
			}
		case newChannel, ok := <-newChannels:
			if !ok {
				newChannels = nil
				continue
			}
			channelType := newChannel.ChannelType()
			handler := channelHandlers[channelType]
			if handler == nil {
				warningLogger.Printf("Unsupported channel type %v", channelType)
				if err := newChannel.Reject(ssh.ConnectionFailed, "open failed"); err != nil {
					warningLogger.Printf("Failed to reject channel: %v", err)
					newChannels = nil
					continue
				}
				continue
			}
			channels.Add(1)
			go func(context channelContext) {
				defer channels.Done()
				if err := handler(newChannel, context); err != nil {
					warningLogger.Printf("Failed to handle new channel: %v", err)
					serverConn.Close()
				}
			}(channelContext{context, channelID})
			channelID++
		}
	}
}
