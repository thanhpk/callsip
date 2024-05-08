package main

import (
	"net"
	"strconv"
	"strings"
	"time"
)

// UDPConn is a UDP connection that allow to set remote address on the fly
// std udp conn require:
//   - client know local port and server's ip and port
//   - server know local port
//
// this udp connection only require local port when create, then
// you can set the remote port whenever you want
type UDPConn struct {
	udpconn *net.UDPConn
	raddr   *net.UDPAddr
}

func (udpConn *UDPConn) Write(b []byte) (int, error) {
	if udpConn == nil {
		return 0, nil
	}
	if udpConn.raddr == nil {
		return 0, nil
	}
	return udpConn.udpconn.WriteToUDP(b, udpConn.raddr)
}

func (udpConn *UDPConn) LocalAddr() net.Addr {
	return udpConn.udpconn.LocalAddr()
}

func (udpConn *UDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, raddr, err := udpConn.udpconn.ReadFromUDP(b)
	if err != nil {
		return n, raddr, err
	}

	// learn from read
	if udpConn.raddr == nil {
		udpConn.raddr = &net.UDPAddr{IP: raddr.IP, Port: raddr.Port}
	} else {
		if raddr.IP.String() != udpConn.raddr.IP.String() || raddr.Port != udpConn.raddr.Port {
			udpConn.raddr = &net.UDPAddr{IP: raddr.IP, Port: raddr.Port}
		}
	}
	return n, raddr, err
}

func (udpConn *UDPConn) Close() error {
	if udpConn == nil {
		return nil
	}
	return udpConn.udpconn.Close()
}

func (udpConn *UDPConn) SetRemoteAddr(addr *net.UDPAddr) {
	udpConn.raddr = addr
}

func (udpConn *UDPConn) SetReadDeadline(deadline time.Time) error {
	if udpConn == nil {
		return nil
	}
	return udpConn.udpconn.SetReadDeadline(deadline)
}

func (udpConn *UDPConn) SetWriteDeadline(deadline time.Time) error {
	if udpConn == nil {
		return nil
	}
	return udpConn.udpconn.SetWriteDeadline(deadline)
}

func FromUDPConn(conn *net.UDPConn) *UDPConn {
	udpConn := &UDPConn{
		udpconn: conn,
	}
	return udpConn
}

func NewUDPConn() (*UDPConn, int) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{
		IP: net.ParseIP("0.0.0.0"),
	})
	if err != nil {
		panic(err)
	}

	udpConn := &UDPConn{udpconn: conn}

	as := strings.Split(conn.LocalAddr().String(), ":")
	port := as[len(as)-1]
	portint, _ := strconv.Atoi(port)
	return udpConn, portint
}
