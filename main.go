// Copyright 2014 Lieven Govaerts. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"code.google.com/p/gopacket"
	"code.google.com/p/gopacket/layers"
	"code.google.com/p/gopacket/pcap"
	"code.google.com/p/gopacket/tcpassembly"
	"code.google.com/p/gopacket/tcpassembly/tcpreader"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"
)

// Command line arguments
var iface = flag.String("ifce", "en0", "Interface to get packets from")
var inputfile = flag.String("infile", "", "read packets from file")
var logAllPackets = flag.Bool("v", false, "Logs every packet in great detail")
var launchCmd = flag.String("e", "", "Launches the command and logs its traffic")
var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")

type BidiStream struct {
	key      uint64
	in, out  *TCPStream
	requests chan *http.Request
}

// TCPStream will handle the actual decoding of http requests and responses.
type TCPStream struct {
	netFlow, tcpFlow gopacket.Flow
	readStream       tcpreader.ReaderStream
	storage          *Storage
	bidikey          uint64
	closed           bool
	reqInProgress    *http.Request
}

// runOut is a blocking function that reads HTTP requests from a stream.
func (h *TCPStream) runOut(bds *BidiStream) {
	buf := bufio.NewReader(&h.readStream)
	var reqID int64

	for {
		/*		_, err := buf.Peek(1)
				if err == io.EOF {
					return
				}*/
		req, err := http.ReadRequest(buf)
		if err == io.EOF {
			//			log.Println("EOF while reading stream", h.netFlow, h.tcpFlow, ":", err)
			// We must read until we see an EOF... very important!
			err = h.storage.CloseTCPConnection(h.bidikey, time.Now())
			if err != nil {
				log.Println("Error storing connection close timestamp", err)
			}
			return
		} else if err != nil {
			tcpreader.DiscardBytesToFirstError(buf)

			if h.closed == true {
				// error occurred after stream was closed, ignore.
			} else {
				log.Println("Error reading stream", h.netFlow, h.tcpFlow, ":", err)
			}
		} else {
			// bodyBytes := tcpreader.DiscardBytesToEOF(req.Body)
			req.Body.Close()
			bds.requests <- req
			err = h.storage.SentRequest(h.bidikey, reqID, time.Now(), req)

			if err != nil {
				log.Println("Error storing request", err)
			}

			reqID++
			//			fmt.Print(".")
			// log.Println("Received request from stream", h.netFlow, h.tcpFlow,
			// 	":", req, "with", bodyBytes, "bytes in request body")
		}
	}
}

// runIn is a blocking function that reads HTTP responses from a stream.
func (h *TCPStream) runIn(bds *BidiStream) {
	buf := bufio.NewReader(&h.readStream)
	var reqID int64

	for {
		// Don't start reading a response if no data is available
		_, err := buf.Peek(1)
		if err == io.EOF {
			return
		}

		// Data available, read response.

		// Find the request to which this is the response.
		req := h.reqInProgress
		if req == nil {
			req = <-bds.requests
			h.reqInProgress = req
		}

		resp, err := http.ReadResponse(buf, req)
		if err == io.EOF {
			// We must read until we see an EOF... very important!
			//			log.Println("EOF while reading stream", h.netFlow, h.tcpFlow, ":", err)
			return
		} else if err != nil {
			tcpreader.DiscardBytesToFirstError(buf)
			if h.closed == true {
				// error occurred after stream was closed, ignore.
			} else {
				log.Println("Error reading stream", h.netFlow, h.tcpFlow, ":", err)
			}
		} else {
			_, err := tcpreader.DiscardBytesToFirstError(resp.Body)
			if err != nil && err != io.EOF {
				log.Println("Error discarding bytes ", err)
			}
			resp.Body.Close()
			err = h.storage.ReceivedResponse(h.bidikey, reqID, time.Now(), resp)
			if err != nil {
				log.Println("Error storing response", err)
			}

			reqID++
			h.reqInProgress = nil

			// fmt.Print(".")
			//log.Println("Received response from stream", h.netFlow, h.tcpFlow,
			//	":", resp, "with", bodyBytes, "bytes in response body")
		}
	}

}

// httpStreamFactory implements tcpassembly.StreamFactory
type httpStreamFactory struct {
	bidiStreams map[uint64]*BidiStream
	storage     *Storage
	closed      bool
}

func NewStreamFactory(s *Storage) *httpStreamFactory {
	return &httpStreamFactory{bidiStreams: make(map[uint64]*BidiStream),
		storage: s}
}

func (h *httpStreamFactory) New(netFlow, tcpFlow gopacket.Flow) tcpassembly.Stream {

	// Watch out: this function can still get called even after all
	// streams were flushed (via FlushAll) and closed.
	/*	if h.closed == true {
			return tcpreader.NewReaderStream()
		}
	*/
	// First the outgoing stream, then the incoming stream
	key := netFlow.FastHash() ^ tcpFlow.FastHash()

	hstream := &TCPStream{
		netFlow:    netFlow,
		tcpFlow:    tcpFlow,
		readStream: tcpreader.NewReaderStream(),
		storage:    h.storage,
		bidikey:    key,
	}

	bds := h.bidiStreams[key]
	if bds == nil {
		//		log.Println("reading stream", netFlow, tcpFlow)
		bds = &BidiStream{out: hstream, key: key,
			requests: make(chan *http.Request, 100)}
		h.bidiStreams[key] = bds
		// Start a coroutine per stream, to ensure that all data is read from
		// the reader stream
		go hstream.runOut(bds)
	} else {
		//		log.Println("opening TCP conn", netFlow, tcpFlow)
		bds.in = hstream
		err := h.storage.OpenTCPConnection(key, time.Now())
		if err != nil {
			log.Println("Error storing connection", err)
		}
		// Start a coroutine per stream, to ensure that all data is read from
		// the reader stream
		go hstream.runIn(bds)
	}

	// ReaderStream implements tcpassembly.Stream, so we can return a pointer to it.
	return &hstream.readStream
}

// LogPacketSize calculates the payload length of a TCP packet and stores it
// in the storage layer.
func (h *httpStreamFactory) LogPacketSize(packet gopacket.Packet) {
	netFlow := packet.NetworkLayer().NetworkFlow()
	tcpFlow := packet.TransportLayer().TransportFlow()
	key := netFlow.FastHash() ^ tcpFlow.FastHash()

	ipv4Layer := packet.Layer(layers.LayerTypeIPv4)
	ipv4, _ := ipv4Layer.(*layers.IPv4)

	tcpLayer := packet.Layer(layers.LayerTypeTCP)
	tcp, _ := tcpLayer.(*layers.TCP)

	bds := h.bidiStreams[key]
	if bds == nil || bds.in == nil || bds.out == nil {
		return
	}

	payloadLength := uint32(ipv4.Length - uint16(ipv4.IHL)*4 - uint16(tcp.DataOffset)*4)

	if bds.in.netFlow == netFlow {
		// This is an incoming packet
		err := h.storage.IncomingTCPPacket(key, payloadLength)
		if err != nil {
			panic(err)
		}
	} else {
		// This is an outgoing packet
		err := h.storage.OutgoingTCPPacket(key, payloadLength)
		if err != nil {
			panic(err)
		}
	}
}

// createProcessEndedChannel creates and returns a channel that will be used
// to return the exit code of command CMD after it's finished.
func createProcessEndedChannel(cmd *exec.Cmd) (cmd_done chan error) {
	cmd_done = make(chan error, 1)
	go func() {
		cmd_done <- cmd.Wait()
	}()
	return cmd_done
}

// createCtrlCchannel creates and returns a channel that will be used to signal
// the user typing CTRL-C.
func createCtrlCchannel() (ctrlc chan os.Signal) {
	ctrlc = make(chan os.Signal, 1)
	signal.Notify(ctrlc, os.Interrupt)
	return
}

// createTimeoutChannel creates and returns a channel, starts a timer of
// duration T and send TRUE over the channel when that durations passes.
func createTimeoutChannel(t time.Duration) (timeout chan bool) {
	timeout = make(chan bool, 1)
	go func() {
		time.Sleep(t * time.Second)
		timeout <- true
	}()
	return
}

func createNetDescChannel() (netDescs chan NetDescriptor) {
	netDescSource := NewOSXNetDescSource()
	netDescs = netDescSource.Descriptors()
	return
}

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	//	log.SetOutput(ioutil.Discard)

	// run the http reader goroutines on all available CPU cores
	runtime.GOMAXPROCS(runtime.NumCPU())

	// Setup profiler
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer f.Close()
		defer pprof.StopCPUProfile()
	}

	// Set up storage layer
	storage, err := NewStorage()
	if err != nil {
		panic(err)
	}

	// Set up assembly
	streamFactory := NewStreamFactory(storage)
	streamPool := tcpassembly.NewStreamPool(streamFactory)
	assembler := tcpassembly.NewAssembler(streamPool)

	// Setup CTRL-C handler channel
	ctrlc := createCtrlCchannel()

	// Setup channel that reports all socket kernel events (Mac OS X only)
	//	netDescs := createNetDescChannel()

	var handle *pcap.Handle
	if *inputfile != "" {
		handle, err = pcap.OpenOffline(*inputfile)
	} else {
		log.Printf("starting capture on interface %q", *iface)
		// Setup packet capture
		handle, err = pcap.OpenLive(*iface, 1600, true, pcap.BlockForever)
	}

	if err != nil {
		panic(err)
	} else if err := handle.SetBPFFilter("tcp and port 80"); err != nil {
		panic(err)
	}
	log.Println("reading in packets. Press CTRL-C to end and report.")
	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	packets := packetSource.Packets()

	// Run the external command
	//	pid := uint32(0)
	var cmd_done chan error
	var start_time time.Time
	if *launchCmd != "" {
		s := *launchCmd
		args := strings.Split(s, " ")
		cmd := exec.Command(args[0], args[1:]...)
		start_time = time.Now()
		cmd_stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Fatal(err)
		}
		err = cmd.Start()
		if err != nil {
			panic(err)
		}
		go io.Copy(os.Stdout, cmd_stdout)
		// Create the channel that listens for the end of the command.
		cmd_done = createProcessEndedChannel(cmd)
		//		pid = uint32(cmd.Process.Pid)
	}

	var timeout chan bool
loop:
	for {
		select {
		/*		case netDesc := <-netDescs:
				if netDesc.Pid == pid {
					log.Println("event received ", netDesc)
				}*/
		case packet, ok := <-packets:
			if !ok {
				log.Println("All data read")
				packets = nil
				timeout = createTimeoutChannel(0)
				break
			}

			if packet == nil {
				break
			}

			if *logAllPackets {
				log.Println(packet)
			}

			if storage.PacketInScope(packet) {
				streamFactory.LogPacketSize(packet)
				netFlow := packet.NetworkLayer().NetworkFlow()
				tcp := packet.TransportLayer().(*layers.TCP)

				assembler.AssembleWithTimestamp(netFlow, tcp,
					packet.Metadata().Timestamp)
			}
		case err := <-cmd_done:
			if err != nil {
				log.Printf("process done with error = %v\n", err)
			}

			log.Println("Process took: ", time.Now().Sub(start_time))

			// Wait for a couple of seconds, just enough to get the events
			// handled by the main function.
			log.Println("Waiting for the remaining responses to arrive.")
			timeout = createTimeoutChannel(10)
		case <-ctrlc:
			// Don't wait.
			timeout = createTimeoutChannel(0)
		case <-timeout:
			break loop
		}
	}

	signal.Stop(ctrlc)

	// Cleanup the go routines
	// Ignore any http request/response parsing errors when closing the streams.
	streamFactory.closed = true
	for _, v := range streamFactory.bidiStreams {
		if v.in != nil {
			v.in.closed = true
		}
		if v.out != nil {
			v.out.closed = true
		}
	}

	assembler.FlushAll()

	// Close the storage layer. This will block until all pending inserts in
	// the db are handled.
	storage.Close()

	reporting, err := NewReporting()
	if err != nil {
		panic(err)
	}

	if err = reporting.Report(); err != nil {
		log.Panic(err)
	}

	os.Exit(0)

}
