package main

import (
	"bytes"
	"fmt"
	"github.com/keroserene/go-webrtc"
	"io"
	"log"
	"net"
	"time"
)

// Implements net.Conn interface
type webRTCConn struct {
	config        *webrtc.Configuration
	pc            *webrtc.PeerConnection
	snowflake     SnowflakeChannel // Interface holding the WebRTC DataChannel.
	broker        *BrokerChannel
	offerChannel  chan *webrtc.SessionDescription
	answerChannel chan *webrtc.SessionDescription
	errorChannel  chan error
	recvPipe      *io.PipeReader
	writePipe     *io.PipeWriter
	buffer        bytes.Buffer
	reset         chan struct{}
	*BytesInfo
}

var webrtcRemote *webRTCConn

func (c *webRTCConn) Read(b []byte) (int, error) {
	return c.recvPipe.Read(b)
}

// Writes bytes out to the snowflake proxy.
func (c *webRTCConn) Write(b []byte) (int, error) {
	c.BytesInfo.AddOutbound(len(b))
	if nil == c.snowflake {
		log.Printf("Buffered %d bytes --> WebRTC", len(b))
		c.buffer.Write(b)
	} else {
		c.snowflake.Send(b)
	}
	return len(b), nil
}

func (c *webRTCConn) Close() error {
	// Data channel closed implicitly?
	return c.pc.Close()
}

func (c *webRTCConn) LocalAddr() net.Addr {
	return nil
}

func (c *webRTCConn) RemoteAddr() net.Addr {
	return nil
}

func (c *webRTCConn) SetDeadline(t time.Time) error {
	return fmt.Errorf("SetDeadline not implemented")
}

func (c *webRTCConn) SetReadDeadline(t time.Time) error {
	return fmt.Errorf("SetReadDeadline not implemented")
}

func (c *webRTCConn) SetWriteDeadline(t time.Time) error {
	return fmt.Errorf("SetWriteDeadline not implemented")
}

func NewWebRTCConnection(config *webrtc.Configuration,
	broker *BrokerChannel) *webRTCConn {
	connection := new(webRTCConn)
	connection.config = config
	connection.broker = broker
	connection.offerChannel = make(chan *webrtc.SessionDescription, 1)
	connection.answerChannel = make(chan *webrtc.SessionDescription, 1)
	connection.errorChannel = make(chan error, 1)
	connection.reset = make(chan struct{}, 1)

	// Log every few seconds.
	connection.BytesInfo = &BytesInfo{
		inboundChan: make(chan int, 5), outboundChan: make(chan int, 5),
		inbound: 0, outbound: 0, inEvents: 0, outEvents: 0,
	}
	go connection.BytesInfo.Log()

	// Pipes remain the same even when DataChannel gets switched.
	connection.recvPipe, connection.writePipe = io.Pipe()

	return connection
}

// WebRTC re-establishment loop. Expected in own goroutine.
func (c *webRTCConn) ConnectLoop() {
	for {
		log.Println("Establishing WebRTC connection...")
		// TODO: When go-webrtc is more stable, it's possible that a new
		// PeerConnection won't need to be re-prepared each time.
		c.preparePeerConnection()
		err := c.establishDataChannel()
		if err == nil {
			c.sendOffer()
			c.receiveAnswer()
			<-c.reset
			log.Println(" --- snowflake connection reset ---")
		} else {
			log.Println("WebRTC: Could not establish DataChannel.")
		}
	}
}

func (c *webRTCConn) preparePeerConnection() {
	if nil != c.pc {
		c.pc.Close()
		c.pc = nil
	}
	pc, err := webrtc.NewPeerConnection(c.config)
	if err != nil {
		log.Printf("NewPeerConnection: %s", err)
		c.errorChannel <- err
	}
	// Prepare PeerConnection callbacks.
	pc.OnNegotiationNeeded = func() {
		log.Println("WebRTC: OnNegotiationNeeded")
		go func() {
			offer, err := pc.CreateOffer()
			// TODO: Potentially timeout and retry if ICE isn't working.
			if err != nil {
				c.errorChannel <- err
				return
			}
			err = pc.SetLocalDescription(offer)
			if err != nil {
				c.errorChannel <- err
				return
			}
		}()
	}
	// Allow candidates to accumulate until OnIceComplete.
	pc.OnIceCandidate = func(candidate webrtc.IceCandidate) {
		log.Printf(candidate.Candidate)
	}
	// TODO: This may soon be deprecated, consider OnIceGatheringStateChange.
	pc.OnIceComplete = func() {
		log.Printf("WebRTC: OnIceComplete")
		c.offerChannel <- pc.LocalDescription()
	}
	// This callback is not expected, as the Client initiates the creation
	// of the data channel, not the remote peer.
	pc.OnDataChannel = func(channel *webrtc.DataChannel) {
		log.Println("OnDataChannel")
		panic("Unexpected OnDataChannel!")
	}
	c.pc = pc
	log.Println("WebRTC: PeerConnection created.")
}

// Create a WebRTC DataChannel locally.
func (c *webRTCConn) establishDataChannel() error {
	dc, err := c.pc.CreateDataChannel("snowflake", webrtc.Init{})
	// Triggers "OnNegotiationNeeded" on the PeerConnection, which will prepare
	// an SDP offer while other goroutines operating on this struct handle the
	// signaling. Eventually fires "OnOpen".
	if err != nil {
		log.Printf("CreateDataChannel: %s", err)
		return err
	}
	dc.OnOpen = func() {
		log.Println("WebRTC: DataChannel.OnOpen")
		if nil != c.snowflake {
			panic("PeerConnection snowflake already exists.")
		}
		// Flush the buffer, then enable datachannel.
		dc.Send(c.buffer.Bytes())
		log.Println("Flushed", c.buffer.Len(), "bytes")
		c.buffer.Reset()

		c.snowflake = dc
	}
	dc.OnClose = func() {
		// Disable the DataChannel as a write destination.
		// Future writes will go to the buffer until a new DataChannel is available.
		log.Println("WebRTC: DataChannel.OnClose")
		// Only reset if this OnClose was triggered remotely.
		if nil != c.snowflake {
			c.snowflake = nil
			c.Reset()
		}
	}
	dc.OnMessage = func(msg []byte) {
		if len(msg) <= 0 {
			log.Println("0 length---")
		}
		c.BytesInfo.AddInbound(len(msg))
		n, err := c.writePipe.Write(msg)
		if err != nil {
			// TODO: Maybe shouldn't actually close.
			log.Println("Error writing to SOCKS pipe")
			c.writePipe.CloseWithError(err)
		}
		if n != len(msg) {
			panic("short write")
		}
	}
	log.Println("WebRTC: DataChannel created.")
	return nil
}

// Block until an offer is available, then send it to either
// the Broker or signal pipe.
func (c *webRTCConn) sendOffer() error {
	log.Println("sendOffer...")
	select {
	case offer := <-c.offerChannel:
		if "" == brokerURL {
			log.Printf("Please Copy & Paste the following to the peer:")
			log.Printf("----------------")
			log.Printf("\n" + offer.Serialize() + "\n")
			log.Printf("----------------")
			return nil
		}
		// Otherwise, use Broker.
		go func() {
			log.Println("Sending offer via BrokerChannel...\nTarget URL: ", brokerURL,
				"\nFront URL:  ", frontDomain)
			answer, err := c.broker.Negotiate(c.pc.LocalDescription())
			if nil != err || nil == answer {
				log.Printf("BrokerChannel error: %s", err)
				answer = nil
			}
			c.answerChannel <- answer
		}()
	case err := <-c.errorChannel:
		c.pc.Close()
		return err
	}
	return nil
}

func (c *webRTCConn) receiveAnswer() {
	go func() {
		answer, ok := <-c.answerChannel
		if !ok || nil == answer {
			log.Printf("Failed to retrieve answer. Retrying in %d seconds", ReconnectTimeout)
			<-time.After(time.Second * ReconnectTimeout)
			c.Reset()
			return
		}
		log.Printf("Received Answer:\n\n%s\n", answer.Sdp)
		err := c.pc.SetRemoteDescription(answer)
		if nil != err {
			c.errorChannel <- err
		}
	}()
}

func (c *webRTCConn) Reset() {
	go func() {
		c.reset <- struct{}{} // Attempt to negotiate a new datachannel..
		log.Println("WebRTC resetting...")
	}()
}
