package quic

import (
	"bytes"
	"context"
	"errors"
	"net"
	"runtime/pprof"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/golang/mock/gomock"
	"github.com/lucas-clemente/quic-go/internal/ackhandler"
	"github.com/lucas-clemente/quic-go/internal/handshake"
	"github.com/lucas-clemente/quic-go/internal/mocks"
	mockackhandler "github.com/lucas-clemente/quic-go/internal/mocks/ackhandler"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/qerr"
	"github.com/lucas-clemente/quic-go/internal/utils"
	"github.com/lucas-clemente/quic-go/internal/wire"
)

type mockConnection struct {
	remoteAddr net.Addr
	localAddr  net.Addr
	written    chan []byte
}

func newMockConnection() *mockConnection {
	return &mockConnection{
		remoteAddr: &net.UDPAddr{},
		written:    make(chan []byte, 100),
	}
}

func (m *mockConnection) Write(p []byte) error {
	b := make([]byte, len(p))
	copy(b, p)
	select {
	case m.written <- b:
	default:
		panic("mockConnection channel full")
	}
	return nil
}
func (m *mockConnection) Read([]byte) (int, net.Addr, error) { panic("not implemented") }

func (m *mockConnection) SetCurrentRemoteAddr(addr net.Addr) {
	m.remoteAddr = addr
}
func (m *mockConnection) LocalAddr() net.Addr  { return m.localAddr }
func (m *mockConnection) RemoteAddr() net.Addr { return m.remoteAddr }
func (*mockConnection) Close() error           { panic("not implemented") }

func areSessionsRunning() bool {
	var b bytes.Buffer
	pprof.Lookup("goroutine").WriteTo(&b, 1)
	return strings.Contains(b.String(), "quic-go.(*session).run")
}

func insertPacketBuffer(p *receivedPacket) *receivedPacket {
	p.buffer = getPacketBuffer()
	return p
}

var _ = Describe("Session", func() {
	var (
		sess          *session
		sessionRunner *MockSessionRunner
		mconn         *mockConnection
		streamManager *MockStreamManager
		packer        *MockPacker
		cryptoSetup   *mocks.MockCryptoSetup
	)

	BeforeEach(func() {
		Eventually(areSessionsRunning).Should(BeFalse())

		sessionRunner = NewMockSessionRunner(mockCtrl)
		mconn = newMockConnection()
		var pSess Session
		var err error
		pSess, err = newSession(
			mconn,
			sessionRunner,
			protocol.ConnectionID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			protocol.ConnectionID{8, 7, 6, 5, 4, 3, 2, 1},
			protocol.ConnectionID{1, 2, 3, 4, 5, 6, 7, 8},
			populateServerConfig(&Config{}),
			nil, // tls.Config
			&handshake.TransportParameters{},
			utils.DefaultLogger,
			protocol.VersionTLS,
		)
		Expect(err).NotTo(HaveOccurred())
		sess = pSess.(*session)
		streamManager = NewMockStreamManager(mockCtrl)
		sess.streamsMap = streamManager
		packer = NewMockPacker(mockCtrl)
		sess.packer = packer
		cryptoSetup = mocks.NewMockCryptoSetup(mockCtrl)
		sess.cryptoStreamHandler = cryptoSetup
	})

	AfterEach(func() {
		Eventually(areSessionsRunning).Should(BeFalse())
	})

	Context("frame handling", func() {
		Context("handling STREAM frames", func() {
			It("passes STREAM frames to the stream", func() {
				f := &wire.StreamFrame{
					StreamID: 5,
					Data:     []byte{0xde, 0xca, 0xfb, 0xad},
				}
				str := NewMockReceiveStreamI(mockCtrl)
				str.EXPECT().handleStreamFrame(f)
				streamManager.EXPECT().GetOrOpenReceiveStream(protocol.StreamID(5)).Return(str, nil)
				err := sess.handleStreamFrame(f, protocol.Encryption1RTT)
				Expect(err).ToNot(HaveOccurred())
			})

			It("returns errors", func() {
				testErr := errors.New("test err")
				f := &wire.StreamFrame{
					StreamID: 5,
					Data:     []byte{0xde, 0xca, 0xfb, 0xad},
				}
				str := NewMockReceiveStreamI(mockCtrl)
				str.EXPECT().handleStreamFrame(f).Return(testErr)
				streamManager.EXPECT().GetOrOpenReceiveStream(protocol.StreamID(5)).Return(str, nil)
				err := sess.handleStreamFrame(f, protocol.Encryption1RTT)
				Expect(err).To(MatchError(testErr))
			})

			It("ignores STREAM frames for closed streams", func() {
				streamManager.EXPECT().GetOrOpenReceiveStream(protocol.StreamID(5)).Return(nil, nil) // for closed streams, the streamManager returns nil
				err := sess.handleStreamFrame(&wire.StreamFrame{
					StreamID: 5,
					Data:     []byte("foobar"),
				}, protocol.Encryption1RTT)
				Expect(err).ToNot(HaveOccurred())
			})

			It("does not accept STREAM frames in non-1RTT packets", func() {
				err := sess.handleStreamFrame(&wire.StreamFrame{
					StreamID: 3,
					Data:     []byte("foobar"),
				}, protocol.EncryptionHandshake)
				Expect(err).To(MatchError(qerr.Error(qerr.UnencryptedStreamData, "received unencrypted stream data on stream 3")))
			})
		})

		Context("handling ACK frames", func() {
			It("informs the SentPacketHandler about ACKs", func() {
				f := &wire.AckFrame{AckRanges: []wire.AckRange{{Smallest: 2, Largest: 3}}}
				sph := mockackhandler.NewMockSentPacketHandler(mockCtrl)
				sph.EXPECT().ReceivedAck(f, protocol.PacketNumber(42), protocol.EncryptionHandshake, gomock.Any())
				sess.sentPacketHandler = sph
				err := sess.handleAckFrame(f, 42, protocol.EncryptionHandshake)
				Expect(err).ToNot(HaveOccurred())
			})

			It("tells the ReceivedPacketHandler to ignore low ranges", func() {
				ack := &wire.AckFrame{AckRanges: []wire.AckRange{{Smallest: 2, Largest: 3}}}
				sph := mockackhandler.NewMockSentPacketHandler(mockCtrl)
				sph.EXPECT().ReceivedAck(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any())
				sph.EXPECT().GetLowestPacketNotConfirmedAcked().Return(protocol.PacketNumber(0x42))
				sess.sentPacketHandler = sph
				rph := mockackhandler.NewMockReceivedPacketHandler(mockCtrl)
				rph.EXPECT().IgnoreBelow(protocol.PacketNumber(0x42))
				sess.receivedPacketHandler = rph
				Expect(sess.handleAckFrame(ack, 0, protocol.Encryption1RTT)).To(Succeed())
			})
		})

		Context("handling RESET_STREAM frames", func() {
			It("closes the streams for writing", func() {
				f := &wire.ResetStreamFrame{
					StreamID:   555,
					ErrorCode:  42,
					ByteOffset: 0x1337,
				}
				str := NewMockReceiveStreamI(mockCtrl)
				streamManager.EXPECT().GetOrOpenReceiveStream(protocol.StreamID(555)).Return(str, nil)
				str.EXPECT().handleResetStreamFrame(f)
				err := sess.handleResetStreamFrame(f)
				Expect(err).ToNot(HaveOccurred())
			})

			It("returns errors", func() {
				f := &wire.ResetStreamFrame{
					StreamID:   7,
					ByteOffset: 0x1337,
				}
				testErr := errors.New("flow control violation")
				str := NewMockReceiveStreamI(mockCtrl)
				streamManager.EXPECT().GetOrOpenReceiveStream(protocol.StreamID(7)).Return(str, nil)
				str.EXPECT().handleResetStreamFrame(f).Return(testErr)
				err := sess.handleResetStreamFrame(f)
				Expect(err).To(MatchError(testErr))
			})

			It("ignores RESET_STREAM frames for closed streams", func() {
				streamManager.EXPECT().GetOrOpenReceiveStream(protocol.StreamID(3)).Return(nil, nil)
				Expect(sess.handleFrame(&wire.ResetStreamFrame{
					StreamID:  3,
					ErrorCode: 42,
				}, 0, protocol.EncryptionUnspecified)).To(Succeed())
			})
		})

		Context("handling MAX_DATA and MAX_STREAM_DATA frames", func() {
			var connFC *mocks.MockConnectionFlowController

			BeforeEach(func() {
				connFC = mocks.NewMockConnectionFlowController(mockCtrl)
				sess.connFlowController = connFC
			})

			It("updates the flow control window of a stream", func() {
				f := &wire.MaxStreamDataFrame{
					StreamID:   12345,
					ByteOffset: 0x1337,
				}
				str := NewMockSendStreamI(mockCtrl)
				streamManager.EXPECT().GetOrOpenSendStream(protocol.StreamID(12345)).Return(str, nil)
				str.EXPECT().handleMaxStreamDataFrame(f)
				err := sess.handleMaxStreamDataFrame(f)
				Expect(err).ToNot(HaveOccurred())
			})

			It("updates the flow control window of the connection", func() {
				offset := protocol.ByteCount(0x800000)
				connFC.EXPECT().UpdateSendWindow(offset)
				sess.handleMaxDataFrame(&wire.MaxDataFrame{ByteOffset: offset})
			})

			It("ignores MAX_STREAM_DATA frames for a closed stream", func() {
				streamManager.EXPECT().GetOrOpenSendStream(protocol.StreamID(10)).Return(nil, nil)
				Expect(sess.handleFrame(&wire.MaxStreamDataFrame{
					StreamID:   10,
					ByteOffset: 1337,
				}, 0, protocol.EncryptionUnspecified)).To(Succeed())
			})
		})

		Context("handling MAX_STREAM_ID frames", func() {
			It("passes the frame to the streamsMap", func() {
				f := &wire.MaxStreamsFrame{
					Type:       protocol.StreamTypeUni,
					MaxStreams: 10,
				}
				streamManager.EXPECT().HandleMaxStreamsFrame(f)
				err := sess.handleMaxStreamsFrame(f)
				Expect(err).ToNot(HaveOccurred())
			})

			It("returns errors", func() {
				f := &wire.MaxStreamsFrame{MaxStreams: 10}
				testErr := errors.New("test error")
				streamManager.EXPECT().HandleMaxStreamsFrame(f).Return(testErr)
				err := sess.handleMaxStreamsFrame(f)
				Expect(err).To(MatchError(testErr))
			})
		})

		Context("handling STOP_SENDING frames", func() {
			It("passes the frame to the stream", func() {
				f := &wire.StopSendingFrame{
					StreamID:  5,
					ErrorCode: 10,
				}
				str := NewMockSendStreamI(mockCtrl)
				streamManager.EXPECT().GetOrOpenSendStream(protocol.StreamID(5)).Return(str, nil)
				str.EXPECT().handleStopSendingFrame(f)
				err := sess.handleStopSendingFrame(f)
				Expect(err).ToNot(HaveOccurred())
			})

			It("ignores STOP_SENDING frames for a closed stream", func() {
				streamManager.EXPECT().GetOrOpenSendStream(protocol.StreamID(3)).Return(nil, nil)
				Expect(sess.handleFrame(&wire.StopSendingFrame{
					StreamID:  3,
					ErrorCode: 1337,
				}, 0, protocol.EncryptionUnspecified)).To(Succeed())
			})
		})

		It("handles PING frames", func() {
			err := sess.handleFrame(&wire.PingFrame{}, 0, protocol.EncryptionUnspecified)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects PATH_RESPONSE frames", func() {
			err := sess.handleFrame(&wire.PathResponseFrame{Data: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}}, 0, protocol.EncryptionUnspecified)
			Expect(err).To(MatchError("unexpected PATH_RESPONSE frame"))
		})

		It("handles PATH_CHALLENGE frames", func() {
			data := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
			err := sess.handleFrame(&wire.PathChallengeFrame{Data: data}, 0, protocol.EncryptionUnspecified)
			Expect(err).ToNot(HaveOccurred())
			frames, _ := sess.framer.AppendControlFrames(nil, 1000)
			Expect(frames).To(Equal([]wire.Frame{&wire.PathResponseFrame{Data: data}}))
		})

		It("handles BLOCKED frames", func() {
			err := sess.handleFrame(&wire.DataBlockedFrame{}, 0, protocol.EncryptionUnspecified)
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles STREAM_BLOCKED frames", func() {
			err := sess.handleFrame(&wire.StreamDataBlockedFrame{}, 0, protocol.EncryptionUnspecified)
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles STREAM_ID_BLOCKED frames", func() {
			err := sess.handleFrame(&wire.StreamsBlockedFrame{}, 0, protocol.EncryptionUnspecified)
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles CONNECTION_CLOSE frames", func() {
			testErr := qerr.Error(qerr.ProofInvalid, "foobar")
			streamManager.EXPECT().CloseWithError(testErr)
			sessionRunner.EXPECT().removeConnectionID(gomock.Any())
			cryptoSetup.EXPECT().Close()

			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				err := sess.run()
				Expect(err).To(MatchError(testErr))
			}()
			err := sess.handleFrame(&wire.ConnectionCloseFrame{ErrorCode: qerr.ProofInvalid, ReasonPhrase: "foobar"}, 0, protocol.EncryptionUnspecified)
			Expect(err).NotTo(HaveOccurred())
			Eventually(sess.Context().Done()).Should(BeClosed())
		})
	})

	It("tells its versions", func() {
		sess.version = 4242
		Expect(sess.GetVersion()).To(Equal(protocol.VersionNumber(4242)))
	})

	It("accepts new streams", func() {
		mstr := NewMockStreamI(mockCtrl)
		streamManager.EXPECT().AcceptStream().Return(mstr, nil)
		str, err := sess.AcceptStream()
		Expect(err).ToNot(HaveOccurred())
		Expect(str).To(Equal(mstr))
	})

	It("drops Retry packets", func() {
		hdr := wire.Header{
			IsLongHeader: true,
			Type:         protocol.PacketTypeRetry,
		}
		buf := &bytes.Buffer{}
		(&wire.ExtendedHeader{Header: hdr}).Write(buf, sess.version)
		Expect(sess.handlePacketImpl(&receivedPacket{
			hdr:    &hdr,
			data:   buf.Bytes(),
			buffer: getPacketBuffer(),
		})).To(BeFalse())
	})

	Context("closing", func() {
		var (
			runErr         error
			expectedRunErr error
		)

		BeforeEach(func() {
			Eventually(areSessionsRunning).Should(BeFalse())
			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				runErr = sess.run()
			}()
			Eventually(areSessionsRunning).Should(BeTrue())
			expectedRunErr = nil
		})

		AfterEach(func() {
			if expectedRunErr != nil {
				Expect(runErr).To(MatchError(expectedRunErr))
			}
		})

		It("shuts down without error", func() {
			streamManager.EXPECT().CloseWithError(qerr.Error(qerr.PeerGoingAway, ""))
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			cryptoSetup.EXPECT().Close()
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{raw: []byte("connection close")}, nil)
			Expect(sess.Close()).To(Succeed())
			Eventually(areSessionsRunning).Should(BeFalse())
			Expect(mconn.written).To(HaveLen(1))
			Expect(mconn.written).To(Receive(ContainSubstring("connection close")))
			Expect(sess.Context().Done()).To(BeClosed())
		})

		It("only closes once", func() {
			streamManager.EXPECT().CloseWithError(qerr.Error(qerr.PeerGoingAway, ""))
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			cryptoSetup.EXPECT().Close()
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
			Expect(sess.Close()).To(Succeed())
			Expect(sess.Close()).To(Succeed())
			Eventually(areSessionsRunning).Should(BeFalse())
			Expect(mconn.written).To(HaveLen(1))
			Expect(sess.Context().Done()).To(BeClosed())
		})

		It("closes streams with proper error", func() {
			testErr := errors.New("test error")
			streamManager.EXPECT().CloseWithError(qerr.Error(0x1337, testErr.Error()))
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			cryptoSetup.EXPECT().Close()
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
			sess.CloseWithError(0x1337, testErr)
			Eventually(areSessionsRunning).Should(BeFalse())
			Expect(sess.Context().Done()).To(BeClosed())
		})

		It("closes the session in order to recreate it", func() {
			streamManager.EXPECT().CloseWithError(gomock.Any())
			sessionRunner.EXPECT().removeConnectionID(gomock.Any())
			cryptoSetup.EXPECT().Close()
			sess.closeForRecreating()
			Expect(mconn.written).To(BeEmpty()) // no CONNECTION_CLOSE or PUBLIC_RESET sent
			Eventually(areSessionsRunning).Should(BeFalse())
			expectedRunErr = errCloseForRecreating
		})

		It("destroys the session", func() {
			testErr := errors.New("close")
			streamManager.EXPECT().CloseWithError(gomock.Any())
			sessionRunner.EXPECT().removeConnectionID(gomock.Any())
			cryptoSetup.EXPECT().Close()
			sess.destroy(testErr)
			Eventually(areSessionsRunning).Should(BeFalse())
			Expect(mconn.written).To(BeEmpty()) // no CONNECTION_CLOSE or PUBLIC_RESET sent
			expectedRunErr = testErr
		})

		It("cancels the context when the run loop exists", func() {
			streamManager.EXPECT().CloseWithError(gomock.Any())
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			cryptoSetup.EXPECT().Close()
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
			returned := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				ctx := sess.Context()
				<-ctx.Done()
				Expect(ctx.Err()).To(MatchError(context.Canceled))
				close(returned)
			}()
			Consistently(returned).ShouldNot(BeClosed())
			sess.Close()
			Eventually(returned).Should(BeClosed())
		})

		It("retransmits the CONNECTION_CLOSE packet if packets are arriving late", func() {
			streamManager.EXPECT().CloseWithError(gomock.Any())
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			cryptoSetup.EXPECT().Close()
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{raw: []byte("foobar")}, nil)
			sess.Close()
			Expect(mconn.written).To(Receive(Equal([]byte("foobar")))) // receive the CONNECTION_CLOSE
			Eventually(sess.Context().Done()).Should(BeClosed())
			for i := 1; i <= 20; i++ {
				sess.handlePacket(&receivedPacket{})
				if i == 1 || i == 2 || i == 4 || i == 8 || i == 16 {
					Expect(mconn.written).To(Receive(Equal([]byte("foobar")))) // receive the CONNECTION_CLOSE
				} else {
					Expect(mconn.written).To(HaveLen(0))
				}
			}
		})
	})

	Context("receiving packets", func() {
		var unpacker *MockUnpacker

		BeforeEach(func() {
			unpacker = NewMockUnpacker(mockCtrl)
			sess.unpacker = unpacker
		})

		getData := func(extHdr *wire.ExtendedHeader) []byte {
			buf := &bytes.Buffer{}
			Expect(extHdr.Write(buf, sess.version)).To(Succeed())
			// need to set extHdr.Header, since the wire.Header contains the parsed length
			hdr, _, _, err := wire.ParsePacket(buf.Bytes(), 0)
			Expect(err).ToNot(HaveOccurred())
			extHdr.Header = *hdr
			return buf.Bytes()
		}

		It("informs the ReceivedPacketHandler about non-retransmittable packets", func() {
			hdr := &wire.ExtendedHeader{
				PacketNumber:    0x37,
				PacketNumberLen: protocol.PacketNumberLen1,
			}
			rcvTime := time.Now().Add(-10 * time.Second)
			unpacker.EXPECT().Unpack(gomock.Any(), gomock.Any()).Return(&unpackedPacket{
				packetNumber:    0x1337,
				encryptionLevel: protocol.EncryptionInitial,
				hdr:             hdr,
				data:            []byte{0}, // one PADDING frame
			}, nil)
			rph := mockackhandler.NewMockReceivedPacketHandler(mockCtrl)
			rph.EXPECT().ReceivedPacket(protocol.PacketNumber(0x1337), protocol.EncryptionInitial, rcvTime, false)
			sess.receivedPacketHandler = rph
			Expect(sess.handlePacketImpl(insertPacketBuffer(&receivedPacket{
				rcvTime: rcvTime,
				hdr:     &hdr.Header,
				data:    getData(hdr),
			}))).To(BeTrue())
		})

		It("informs the ReceivedPacketHandler about retransmittable packets", func() {
			hdr := &wire.ExtendedHeader{
				PacketNumber:    0x37,
				PacketNumberLen: protocol.PacketNumberLen1,
			}
			rcvTime := time.Now().Add(-10 * time.Second)
			buf := &bytes.Buffer{}
			Expect((&wire.PingFrame{}).Write(buf, sess.version)).To(Succeed())
			unpacker.EXPECT().Unpack(gomock.Any(), gomock.Any()).Return(&unpackedPacket{
				packetNumber:    0x1337,
				encryptionLevel: protocol.EncryptionHandshake,
				hdr:             hdr,
				data:            buf.Bytes(),
			}, nil)
			rph := mockackhandler.NewMockReceivedPacketHandler(mockCtrl)
			rph.EXPECT().ReceivedPacket(protocol.PacketNumber(0x1337), protocol.EncryptionHandshake, rcvTime, true)
			sess.receivedPacketHandler = rph
			Expect(sess.handlePacketImpl(insertPacketBuffer(&receivedPacket{
				rcvTime: rcvTime,
				hdr:     &hdr.Header,
				data:    getData(hdr),
			}))).To(BeTrue())
		})

		It("drops a packet when unpacking fails", func() {
			unpacker.EXPECT().Unpack(gomock.Any(), gomock.Any()).Return(nil, errors.New("unpack error"))
			streamManager.EXPECT().CloseWithError(gomock.Any())
			cryptoSetup.EXPECT().Close()
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				sess.run()
			}()
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			sess.handlePacket(insertPacketBuffer(&receivedPacket{
				hdr:  &wire.Header{},
				data: getData(&wire.ExtendedHeader{PacketNumberLen: protocol.PacketNumberLen1}),
			}))
			Consistently(sess.Context().Done()).ShouldNot(BeClosed())
			// make the go routine return
			sess.closeLocal(errors.New("close"))
			Eventually(sess.Context().Done()).Should(BeClosed())
		})

		It("rejects packets with empty payload", func() {
			unpacker.EXPECT().Unpack(gomock.Any(), gomock.Any()).Return(&unpackedPacket{
				hdr:  &wire.ExtendedHeader{},
				data: []byte{}, // no payload
			}, nil)
			streamManager.EXPECT().CloseWithError(gomock.Any())
			cryptoSetup.EXPECT().Close()
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
			done := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				err := sess.run()
				Expect(err).To(MatchError(qerr.MissingPayload))
				close(done)
			}()
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			sess.handlePacket(insertPacketBuffer(&receivedPacket{
				hdr:  &wire.Header{},
				data: getData(&wire.ExtendedHeader{PacketNumberLen: protocol.PacketNumberLen1}),
			}))
			Eventually(done).Should(BeClosed())
		})

		It("handles duplicate packets", func() {
			hdr := &wire.ExtendedHeader{
				PacketNumber:    5,
				PacketNumberLen: protocol.PacketNumberLen1,
			}
			unpacker.EXPECT().Unpack(gomock.Any(), gomock.Any()).Return(&unpackedPacket{
				encryptionLevel: protocol.Encryption1RTT,
				hdr:             hdr,
				data:            []byte{0}, // one PADDING frame
			}, nil).Times(2)
			Expect(sess.handlePacketImpl(insertPacketBuffer(&receivedPacket{hdr: &hdr.Header, data: getData(hdr)}))).To(BeTrue())
			Expect(sess.handlePacketImpl(insertPacketBuffer(&receivedPacket{hdr: &hdr.Header, data: getData(hdr)}))).To(BeTrue())
		})

		It("ignores 0-RTT packets", func() {
			Expect(sess.handlePacketImpl(insertPacketBuffer(&receivedPacket{
				hdr: &wire.Header{
					IsLongHeader:     true,
					Type:             protocol.PacketType0RTT,
					DestConnectionID: sess.srcConnID,
				},
			}))).To(BeFalse())
		})

		It("ignores packets with a different source connection ID", func() {
			hdr := &wire.Header{
				IsLongHeader:     true,
				DestConnectionID: sess.destConnID,
				SrcConnectionID:  sess.srcConnID,
				Length:           1,
			}
			// Send one packet, which might change the connection ID.
			// only EXPECT one call to the unpacker
			unpacker.EXPECT().Unpack(gomock.Any(), gomock.Any()).Return(&unpackedPacket{
				encryptionLevel: protocol.Encryption1RTT,
				hdr:             &wire.ExtendedHeader{Header: *hdr},
				data:            []byte{0}, // one PADDING frame
			}, nil)
			Expect(sess.handlePacketImpl(insertPacketBuffer(&receivedPacket{
				hdr:  hdr,
				data: getData(&wire.ExtendedHeader{PacketNumberLen: protocol.PacketNumberLen1}),
			}))).To(BeTrue())
			// The next packet has to be ignored, since the source connection ID doesn't match.
			Expect(sess.handlePacketImpl(insertPacketBuffer(&receivedPacket{
				hdr: &wire.Header{
					IsLongHeader:     true,
					DestConnectionID: sess.destConnID,
					SrcConnectionID:  protocol.ConnectionID{0xde, 0xad, 0xbe, 0xef},
					Length:           1,
				},
				data: getData(&wire.ExtendedHeader{PacketNumberLen: protocol.PacketNumberLen1}),
			}))).To(BeFalse())
		})

		Context("updating the remote address", func() {
			It("doesn't support connection migration", func() {
				unpacker.EXPECT().Unpack(gomock.Any(), gomock.Any()).Return(&unpackedPacket{
					encryptionLevel: protocol.Encryption1RTT,
					hdr:             &wire.ExtendedHeader{},
					data:            []byte{0}, // one PADDING frame
				}, nil)
				origAddr := sess.conn.(*mockConnection).remoteAddr
				remoteIP := &net.IPAddr{IP: net.IPv4(192, 168, 0, 100)}
				Expect(origAddr).ToNot(Equal(remoteIP))
				Expect(sess.handlePacketImpl(insertPacketBuffer(&receivedPacket{
					remoteAddr: remoteIP,
					hdr:        &wire.Header{},
					data:       getData(&wire.ExtendedHeader{PacketNumberLen: protocol.PacketNumberLen1}),
				}))).To(BeTrue())
				Expect(sess.conn.(*mockConnection).remoteAddr).To(Equal(origAddr))
			})
		})
	})

	Context("sending packets", func() {
		getPacket := func(pn protocol.PacketNumber) *packedPacket {
			buffer := getPacketBuffer()
			data := buffer.Slice[:0]
			data = append(data, []byte("foobar")...)
			return &packedPacket{
				raw:    data,
				buffer: buffer,
				header: &wire.ExtendedHeader{PacketNumber: pn},
			}
		}

		It("sends packets", func() {
			packer.EXPECT().PackPacket().Return(getPacket(1), nil)
			Expect(sess.receivedPacketHandler.ReceivedPacket(0x035e, protocol.Encryption1RTT, time.Now(), true)).To(Succeed())
			sent, err := sess.sendPacket()
			Expect(err).NotTo(HaveOccurred())
			Expect(sent).To(BeTrue())
		})

		It("doesn't send packets if there's nothing to send", func() {
			packer.EXPECT().PackPacket().Return(getPacket(2), nil)
			Expect(sess.receivedPacketHandler.ReceivedPacket(0x035e, protocol.Encryption1RTT, time.Now(), true)).To(Succeed())
			sent, err := sess.sendPacket()
			Expect(err).NotTo(HaveOccurred())
			Expect(sent).To(BeTrue())
		})

		It("sends ACK only packets", func() {
			sph := mockackhandler.NewMockSentPacketHandler(mockCtrl)
			sph.EXPECT().GetAlarmTimeout().AnyTimes()
			sph.EXPECT().SendMode().Return(ackhandler.SendAck)
			sph.EXPECT().ShouldSendNumPackets().Return(1000)
			packer.EXPECT().MaybePackAckPacket()
			sess.sentPacketHandler = sph
			Expect(sess.sendPackets()).To(Succeed())
		})

		It("adds a BLOCKED frame when it is connection-level flow control blocked", func() {
			fc := mocks.NewMockConnectionFlowController(mockCtrl)
			fc.EXPECT().IsNewlyBlocked().Return(true, protocol.ByteCount(1337))
			packer.EXPECT().PackPacket().Return(getPacket(1), nil)
			sess.connFlowController = fc
			sent, err := sess.sendPacket()
			Expect(err).NotTo(HaveOccurred())
			Expect(sent).To(BeTrue())
			frames, _ := sess.framer.AppendControlFrames(nil, 1000)
			Expect(frames).To(Equal([]wire.Frame{&wire.DataBlockedFrame{DataLimit: 1337}}))
		})

		It("sends a retransmission and a regular packet in the same run", func() {
			packetToRetransmit := &ackhandler.Packet{
				PacketNumber: 10,
				PacketType:   protocol.PacketTypeHandshake,
			}
			retransmittedPacket := getPacket(123)
			newPacket := getPacket(234)
			sess.windowUpdateQueue.callback(&wire.MaxDataFrame{})
			sph := mockackhandler.NewMockSentPacketHandler(mockCtrl)
			sph.EXPECT().DequeuePacketForRetransmission().Return(packetToRetransmit)
			sph.EXPECT().SendMode().Return(ackhandler.SendRetransmission)
			sph.EXPECT().SendMode().Return(ackhandler.SendAny)
			sph.EXPECT().ShouldSendNumPackets().Return(2)
			sph.EXPECT().TimeUntilSend()
			gomock.InOrder(
				packer.EXPECT().PackRetransmission(packetToRetransmit).Return([]*packedPacket{retransmittedPacket}, nil),
				sph.EXPECT().SentPacketsAsRetransmission(gomock.Any(), protocol.PacketNumber(10)).Do(func(packets []*ackhandler.Packet, _ protocol.PacketNumber) {
					Expect(packets).To(HaveLen(1))
					Expect(packets[0].PacketNumber).To(Equal(protocol.PacketNumber(123)))
				}),
				packer.EXPECT().PackPacket().Return(newPacket, nil),
				sph.EXPECT().SentPacket(gomock.Any()).Do(func(p *ackhandler.Packet) {
					Expect(p.PacketNumber).To(Equal(protocol.PacketNumber(234)))
				}),
			)
			sess.sentPacketHandler = sph
			Expect(sess.sendPackets()).To(Succeed())
		})

		It("sends multiple packets, if the retransmission is split", func() {
			packet := &ackhandler.Packet{
				PacketNumber: 42,
				Frames: []wire.Frame{&wire.StreamFrame{
					StreamID: 0x5,
					Data:     []byte("foobar"),
				}},
				EncryptionLevel: protocol.Encryption1RTT,
			}
			retransmissions := []*packedPacket{getPacket(1337), getPacket(1338)}
			sph := mockackhandler.NewMockSentPacketHandler(mockCtrl)
			sph.EXPECT().DequeuePacketForRetransmission().Return(packet)
			packer.EXPECT().PackRetransmission(packet).Return(retransmissions, nil)
			sph.EXPECT().SentPacketsAsRetransmission(gomock.Any(), protocol.PacketNumber(42)).Do(func(packets []*ackhandler.Packet, _ protocol.PacketNumber) {
				Expect(packets).To(HaveLen(2))
				Expect(packets[0].PacketNumber).To(Equal(protocol.PacketNumber(1337)))
				Expect(packets[1].PacketNumber).To(Equal(protocol.PacketNumber(1338)))
			})
			sess.sentPacketHandler = sph
			sent, err := sess.maybeSendRetransmission()
			Expect(err).NotTo(HaveOccurred())
			Expect(sent).To(BeTrue())
			Expect(mconn.written).To(HaveLen(2))
		})

		It("sends a probe packet", func() {
			packetToRetransmit := &ackhandler.Packet{
				PacketNumber: 0x42,
				PacketType:   protocol.PacketTypeHandshake,
			}
			retransmittedPacket := getPacket(123)
			sph := mockackhandler.NewMockSentPacketHandler(mockCtrl)
			sph.EXPECT().TimeUntilSend()
			sph.EXPECT().SendMode().Return(ackhandler.SendPTO)
			sph.EXPECT().ShouldSendNumPackets().Return(1)
			sph.EXPECT().DequeueProbePacket().Return(packetToRetransmit, nil)
			packer.EXPECT().PackRetransmission(packetToRetransmit).Return([]*packedPacket{retransmittedPacket}, nil)
			sph.EXPECT().SentPacketsAsRetransmission(gomock.Any(), protocol.PacketNumber(0x42)).Do(func(packets []*ackhandler.Packet, _ protocol.PacketNumber) {
				Expect(packets).To(HaveLen(1))
				Expect(packets[0].PacketNumber).To(Equal(protocol.PacketNumber(123)))
			})
			sess.sentPacketHandler = sph
			Expect(sess.sendPackets()).To(Succeed())
		})

		It("doesn't send when the SentPacketHandler doesn't allow it", func() {
			sph := mockackhandler.NewMockSentPacketHandler(mockCtrl)
			sph.EXPECT().SendMode().Return(ackhandler.SendNone)
			sess.sentPacketHandler = sph
			err := sess.sendPackets()
			Expect(err).ToNot(HaveOccurred())
		})

		Context("packet pacing", func() {
			var sph *mockackhandler.MockSentPacketHandler

			BeforeEach(func() {
				sph = mockackhandler.NewMockSentPacketHandler(mockCtrl)
				sph.EXPECT().GetAlarmTimeout().AnyTimes()
				sph.EXPECT().DequeuePacketForRetransmission().AnyTimes()
				sess.sentPacketHandler = sph
				streamManager.EXPECT().CloseWithError(gomock.Any())
			})

			It("sends multiple packets one by one immediately", func() {
				sph.EXPECT().SentPacket(gomock.Any()).Times(2)
				sph.EXPECT().ShouldSendNumPackets().Return(1).Times(2)
				sph.EXPECT().TimeUntilSend().Return(time.Now()).Times(2)
				sph.EXPECT().TimeUntilSend().Return(time.Now().Add(time.Hour))
				sph.EXPECT().SendMode().Return(ackhandler.SendAny).Times(2) // allow 2 packets...
				packer.EXPECT().PackPacket().Return(getPacket(10), nil)
				packer.EXPECT().PackPacket().Return(getPacket(11), nil)
				done := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
					sess.run()
					close(done)
				}()
				sess.scheduleSending()
				Eventually(mconn.written).Should(HaveLen(2))
				Consistently(mconn.written).Should(HaveLen(2))
				// make the go routine return
				packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
				sessionRunner.EXPECT().retireConnectionID(gomock.Any())
				cryptoSetup.EXPECT().Close()
				sess.Close()
				Eventually(done).Should(BeClosed())
			})

			// when becoming congestion limited, at some point the SendMode will change from SendAny to SendAck
			// we shouldn't send the ACK in the same run
			It("doesn't send an ACK right after becoming congestion limited", func() {
				sph.EXPECT().SentPacket(gomock.Any())
				sph.EXPECT().ShouldSendNumPackets().Return(1000)
				sph.EXPECT().TimeUntilSend().Return(time.Now())
				sph.EXPECT().SendMode().Return(ackhandler.SendAny)
				sph.EXPECT().SendMode().Return(ackhandler.SendAck)
				packer.EXPECT().PackPacket().Return(getPacket(100), nil)
				done := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
					sess.run()
					close(done)
				}()
				sess.scheduleSending()
				Eventually(mconn.written).Should(HaveLen(1))
				Consistently(mconn.written).Should(HaveLen(1))
				// make the go routine return
				packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
				sessionRunner.EXPECT().retireConnectionID(gomock.Any())
				cryptoSetup.EXPECT().Close()
				sess.Close()
				Eventually(done).Should(BeClosed())
			})

			It("paces packets", func() {
				pacingDelay := scaleDuration(100 * time.Millisecond)
				sph.EXPECT().SentPacket(gomock.Any()).Times(2)
				sph.EXPECT().TimeUntilSend().Return(time.Now().Add(-time.Minute)) // send one packet immediately
				sph.EXPECT().TimeUntilSend().Return(time.Now().Add(pacingDelay))  // send one
				sph.EXPECT().TimeUntilSend().Return(time.Now().Add(time.Hour))
				sph.EXPECT().ShouldSendNumPackets().Times(2).Return(1)
				sph.EXPECT().SendMode().Return(ackhandler.SendAny).AnyTimes()
				packer.EXPECT().PackPacket().Return(getPacket(100), nil)
				packer.EXPECT().PackPacket().Return(getPacket(101), nil)
				done := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
					sess.run()
					close(done)
				}()
				sess.scheduleSending()
				Eventually(mconn.written).Should(HaveLen(1))
				Consistently(mconn.written, pacingDelay/2).Should(HaveLen(1))
				Eventually(mconn.written, 2*pacingDelay).Should(HaveLen(2))
				// make the go routine return
				packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
				sessionRunner.EXPECT().retireConnectionID(gomock.Any())
				cryptoSetup.EXPECT().Close()
				sess.Close()
				Eventually(done).Should(BeClosed())
			})

			It("sends multiple packets at once", func() {
				sph.EXPECT().SentPacket(gomock.Any()).Times(3)
				sph.EXPECT().ShouldSendNumPackets().Return(3)
				sph.EXPECT().TimeUntilSend().Return(time.Now())
				sph.EXPECT().TimeUntilSend().Return(time.Now().Add(time.Hour))
				sph.EXPECT().SendMode().Return(ackhandler.SendAny).Times(3)
				packer.EXPECT().PackPacket().Return(getPacket(1000), nil)
				packer.EXPECT().PackPacket().Return(getPacket(1001), nil)
				packer.EXPECT().PackPacket().Return(getPacket(1002), nil)
				done := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
					sess.run()
					close(done)
				}()
				sess.scheduleSending()
				Eventually(mconn.written).Should(HaveLen(3))
				// make the go routine return
				packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
				sessionRunner.EXPECT().retireConnectionID(gomock.Any())
				cryptoSetup.EXPECT().Close()
				sess.Close()
				Eventually(done).Should(BeClosed())
			})

			It("doesn't set a pacing timer when there is no data to send", func() {
				sph.EXPECT().TimeUntilSend().Return(time.Now())
				sph.EXPECT().ShouldSendNumPackets().Return(1)
				sph.EXPECT().SendMode().Return(ackhandler.SendAny).AnyTimes()
				packer.EXPECT().PackPacket()
				done := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
					sess.run()
					close(done)
				}()
				sess.scheduleSending() // no packet will get sent
				Consistently(mconn.written).ShouldNot(Receive())
				// make the go routine return
				sessionRunner.EXPECT().retireConnectionID(gomock.Any())
				packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
				cryptoSetup.EXPECT().Close()
				sess.Close()
				Eventually(done).Should(BeClosed())
			})
		})

		Context("scheduling sending", func() {
			It("sends when scheduleSending is called", func() {
				sph := mockackhandler.NewMockSentPacketHandler(mockCtrl)
				sph.EXPECT().GetAlarmTimeout().AnyTimes()
				sph.EXPECT().TimeUntilSend().AnyTimes()
				sph.EXPECT().SendMode().Return(ackhandler.SendAny).AnyTimes()
				sph.EXPECT().ShouldSendNumPackets().AnyTimes().Return(1)
				sph.EXPECT().SentPacket(gomock.Any())
				sess.sentPacketHandler = sph
				packer.EXPECT().PackPacket().Return(getPacket(1), nil)

				go func() {
					defer GinkgoRecover()
					cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
					sess.run()
				}()
				Consistently(mconn.written).ShouldNot(Receive())
				sess.scheduleSending()
				Eventually(mconn.written).Should(Receive())
				// make the go routine return
				sessionRunner.EXPECT().retireConnectionID(gomock.Any())
				streamManager.EXPECT().CloseWithError(gomock.Any())
				packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
				cryptoSetup.EXPECT().Close()
				sess.Close()
				Eventually(sess.Context().Done()).Should(BeClosed())
			})

			It("sets the timer to the ack timer", func() {
				packer.EXPECT().PackPacket().Return(getPacket(1234), nil)
				sph := mockackhandler.NewMockSentPacketHandler(mockCtrl)
				sph.EXPECT().TimeUntilSend().Return(time.Now())
				sph.EXPECT().TimeUntilSend().Return(time.Now().Add(time.Hour))
				sph.EXPECT().GetAlarmTimeout().AnyTimes()
				sph.EXPECT().SendMode().Return(ackhandler.SendAny).AnyTimes()
				sph.EXPECT().ShouldSendNumPackets().Return(1)
				sph.EXPECT().SentPacket(gomock.Any()).Do(func(p *ackhandler.Packet) {
					Expect(p.PacketNumber).To(Equal(protocol.PacketNumber(1234)))
				})
				sess.sentPacketHandler = sph
				rph := mockackhandler.NewMockReceivedPacketHandler(mockCtrl)
				rph.EXPECT().GetAlarmTimeout().Return(time.Now().Add(10 * time.Millisecond))
				// make the run loop wait
				rph.EXPECT().GetAlarmTimeout().Return(time.Now().Add(time.Hour)).MaxTimes(1)
				sess.receivedPacketHandler = rph

				go func() {
					defer GinkgoRecover()
					cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
					sess.run()
				}()
				Eventually(mconn.written).Should(Receive())
				// make sure the go routine returns
				packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
				sessionRunner.EXPECT().retireConnectionID(gomock.Any())
				streamManager.EXPECT().CloseWithError(gomock.Any())
				cryptoSetup.EXPECT().Close()
				sess.Close()
				Eventually(sess.Context().Done()).Should(BeClosed())
			})
		})
	})

	It("closes when RunHandshake() errors", func() {
		testErr := errors.New("crypto setup error")
		streamManager.EXPECT().CloseWithError(qerr.Error(qerr.InternalError, testErr.Error()))
		sessionRunner.EXPECT().retireConnectionID(gomock.Any())
		cryptoSetup.EXPECT().Close()
		packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
		go func() {
			defer GinkgoRecover()
			cryptoSetup.EXPECT().RunHandshake().Return(testErr)
			err := sess.run()
			Expect(err).To(MatchError(testErr))
		}()
		Eventually(sess.Context().Done()).Should(BeClosed())
	})

	It("calls the onHandshakeComplete callback when the handshake completes", func() {
		packer.EXPECT().PackPacket().AnyTimes()
		go func() {
			defer GinkgoRecover()
			sessionRunner.EXPECT().onHandshakeComplete(gomock.Any())
			cryptoSetup.EXPECT().RunHandshake()
			sess.run()
		}()
		Consistently(sess.Context().Done()).ShouldNot(BeClosed())
		// make sure the go routine returns
		sessionRunner.EXPECT().retireConnectionID(gomock.Any())
		streamManager.EXPECT().CloseWithError(gomock.Any())
		packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
		cryptoSetup.EXPECT().Close()
		Expect(sess.Close()).To(Succeed())
		Eventually(sess.Context().Done()).Should(BeClosed())
	})

	It("sends a forward-secure packet when the handshake completes", func() {
		done := make(chan struct{})
		gomock.InOrder(
			sessionRunner.EXPECT().onHandshakeComplete(gomock.Any()),
			packer.EXPECT().PackPacket().DoAndReturn(func() (*packedPacket, error) {
				defer close(done)
				return &packedPacket{
					header: &wire.ExtendedHeader{},
					buffer: getPacketBuffer(),
				}, nil
			}),
			packer.EXPECT().PackPacket().AnyTimes(),
		)
		go func() {
			defer GinkgoRecover()
			cryptoSetup.EXPECT().RunHandshake()
			sess.run()
		}()
		Eventually(done).Should(BeClosed())
		//make sure the go routine returns
		streamManager.EXPECT().CloseWithError(gomock.Any())
		sessionRunner.EXPECT().retireConnectionID(gomock.Any())
		packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
		cryptoSetup.EXPECT().Close()
		Expect(sess.Close()).To(Succeed())
		Eventually(sess.Context().Done()).Should(BeClosed())
	})

	It("doesn't return a run error when closing", func() {
		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
			Expect(sess.run()).To(Succeed())
			close(done)
		}()
		streamManager.EXPECT().CloseWithError(gomock.Any())
		sessionRunner.EXPECT().retireConnectionID(gomock.Any())
		packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
		cryptoSetup.EXPECT().Close()
		Expect(sess.Close()).To(Succeed())
		Eventually(done).Should(BeClosed())
	})

	It("passes errors to the session runner", func() {
		testErr := errors.New("handshake error")
		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
			err := sess.run()
			Expect(err).To(MatchError(qerr.Error(0x1337, testErr.Error())))
			close(done)
		}()
		streamManager.EXPECT().CloseWithError(gomock.Any())
		sessionRunner.EXPECT().retireConnectionID(gomock.Any())
		packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
		cryptoSetup.EXPECT().Close()
		Expect(sess.CloseWithError(0x1337, testErr)).To(Succeed())
		Eventually(done).Should(BeClosed())
	})

	Context("transport parameters", func() {
		It("errors if it can't unmarshal the TransportParameters", func() {
			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				err := sess.run()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("transport parameter"))
			}()
			streamManager.EXPECT().CloseWithError(gomock.Any())
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
			cryptoSetup.EXPECT().Close()
			sess.processTransportParameters([]byte("invalid"))
			Eventually(sess.Context().Done()).Should(BeClosed())
		})

		It("process transport parameters received from the client", func() {
			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				sess.run()
			}()
			params := &handshake.TransportParameters{
				IdleTimeout:                   90 * time.Second,
				InitialMaxStreamDataBidiLocal: 0x5000,
				InitialMaxData:                0x5000,
				// marshaling always sets it to this value
				MaxPacketSize: protocol.MaxReceivePacketSize,
			}
			chtp := &handshake.ClientHelloTransportParameters{
				InitialVersion: sess.version,
				Parameters:     *params,
			}
			streamManager.EXPECT().UpdateLimits(params)
			packer.EXPECT().HandleTransportParameters(params)
			sess.processTransportParameters(chtp.Marshal())
			// make the go routine return
			streamManager.EXPECT().CloseWithError(gomock.Any())
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
			cryptoSetup.EXPECT().Close()
			sess.Close()
			Eventually(sess.Context().Done()).Should(BeClosed())
		})

		It("accepts a valid version negotiation", func() {
			sess.version = 42
			sess.config.Versions = []protocol.VersionNumber{13, 37, 42}
			chtp := &handshake.ClientHelloTransportParameters{
				InitialVersion: 22, // this must be an unsupported version
			}
			_, err := sess.processTransportParametersForServer(chtp.Marshal())
			Expect(err).ToNot(HaveOccurred())
		})

		It("erros when a version negotiation was performed, although we already support the initial version", func() {
			sess.version = 42
			sess.config.Versions = []protocol.VersionNumber{13, 37, 42}
			chtp := &handshake.ClientHelloTransportParameters{
				InitialVersion: 13, // this must be a supported version
			}
			_, err := sess.processTransportParametersForServer(chtp.Marshal())
			Expect(err).To(MatchError("VersionNegotiationMismatch: Client should have used the initial version"))
		})
	})

	Context("keep-alives", func() {
		// should be shorter than the local timeout for these tests
		// otherwise we'd send a CONNECTION_CLOSE in the tests where we're testing that no PING is sent
		remoteIdleTimeout := 20 * time.Second

		BeforeEach(func() {
			sess.peerParams = &handshake.TransportParameters{IdleTimeout: remoteIdleTimeout}
		})

		It("sends a PING", func() {
			sess.handshakeComplete = true
			sess.config.KeepAlive = true
			sess.lastNetworkActivityTime = time.Now().Add(-remoteIdleTimeout / 2)
			sent := make(chan struct{})
			packer.EXPECT().PackPacket().Do(func() (*packedPacket, error) {
				close(sent)
				return nil, nil
			})
			done := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				sess.run()
				close(done)
			}()
			Eventually(sent).Should(BeClosed())
			// make the go routine return
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			streamManager.EXPECT().CloseWithError(gomock.Any())
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
			cryptoSetup.EXPECT().Close()
			sess.Close()
			Eventually(done).Should(BeClosed())
		})

		It("doesn't send a PING packet if keep-alive is disabled", func() {
			sess.handshakeComplete = true
			sess.config.KeepAlive = false
			sess.lastNetworkActivityTime = time.Now().Add(-remoteIdleTimeout / 2)
			done := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				sess.run()
				close(done)
			}()
			Consistently(mconn.written).ShouldNot(Receive())
			// make the go routine return
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			streamManager.EXPECT().CloseWithError(gomock.Any())
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
			cryptoSetup.EXPECT().Close()
			sess.Close()
			Eventually(done).Should(BeClosed())
		})

		It("doesn't send a PING if the handshake isn't completed yet", func() {
			sess.handshakeComplete = false
			sess.config.KeepAlive = true
			sess.lastNetworkActivityTime = time.Now().Add(-remoteIdleTimeout / 2)
			done := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				sess.run()
				close(done)
			}()
			Consistently(mconn.written).ShouldNot(Receive())
			// make the go routine return
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			streamManager.EXPECT().CloseWithError(gomock.Any())
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
			cryptoSetup.EXPECT().Close()
			sess.Close()
			Eventually(done).Should(BeClosed())
		})
	})

	Context("timeouts", func() {
		BeforeEach(func() {
			streamManager.EXPECT().CloseWithError(gomock.Any())
		})

		It("times out due to no network activity", func() {
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			sess.handshakeComplete = true
			sess.lastNetworkActivityTime = time.Now().Add(-time.Hour)
			done := make(chan struct{})
			cryptoSetup.EXPECT().Close()
			packer.EXPECT().PackConnectionClose(gomock.Any()).DoAndReturn(func(f *wire.ConnectionCloseFrame) (*packedPacket, error) {
				Expect(f.ErrorCode).To(Equal(qerr.NetworkIdleTimeout))
				return &packedPacket{}, nil
			})
			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				err := sess.run()
				Expect(err.(*qerr.QuicError).ErrorCode).To(Equal(qerr.NetworkIdleTimeout))
				close(done)
			}()
			Eventually(done).Should(BeClosed())
		})

		It("times out due to non-completed handshake", func() {
			sess.sessionCreationTime = time.Now().Add(-protocol.DefaultHandshakeTimeout).Add(-time.Second)
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			cryptoSetup.EXPECT().Close()
			packer.EXPECT().PackConnectionClose(gomock.Any()).DoAndReturn(func(f *wire.ConnectionCloseFrame) (*packedPacket, error) {
				Expect(f.ErrorCode).To(Equal(qerr.HandshakeTimeout))
				return &packedPacket{}, nil
			})
			done := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				err := sess.run()
				Expect(err.(*qerr.QuicError).ErrorCode).To(Equal(qerr.HandshakeTimeout))
				close(done)
			}()
			Eventually(done).Should(BeClosed())
		})

		It("does not use the idle timeout before the handshake complete", func() {
			sess.config.IdleTimeout = 9999 * time.Second
			sess.lastNetworkActivityTime = time.Now().Add(-time.Minute)
			packer.EXPECT().PackConnectionClose(gomock.Any()).DoAndReturn(func(f *wire.ConnectionCloseFrame) (*packedPacket, error) {
				Expect(f.ErrorCode).To(Equal(qerr.PeerGoingAway))
				return &packedPacket{}, nil
			})
			// the handshake timeout is irrelevant here, since it depends on the time the session was created,
			// and not on the last network activity
			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				sess.run()
			}()
			Consistently(sess.Context().Done()).ShouldNot(BeClosed())
			// make the go routine return
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			cryptoSetup.EXPECT().Close()
			sess.Close()
			Eventually(sess.Context().Done()).Should(BeClosed())
		})

		It("closes the session due to the idle timeout after handshake", func() {
			packer.EXPECT().PackPacket().AnyTimes()
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			cryptoSetup.EXPECT().Close()
			packer.EXPECT().PackConnectionClose(gomock.Any()).DoAndReturn(func(f *wire.ConnectionCloseFrame) (*packedPacket, error) {
				Expect(f.ErrorCode).To(Equal(qerr.NetworkIdleTimeout))
				return &packedPacket{}, nil
			})
			sess.config.IdleTimeout = 0
			done := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				sessionRunner.EXPECT().onHandshakeComplete(sess)
				cryptoSetup.EXPECT().RunHandshake()
				err := sess.run()
				Expect(err.(*qerr.QuicError).ErrorCode).To(Equal(qerr.NetworkIdleTimeout))
				close(done)
			}()
			Eventually(done).Should(BeClosed())
		})
	})

	It("stores up to MaxSessionUnprocessedPackets packets", func(done Done) {
		// Nothing here should block
		for i := protocol.PacketNumber(0); i < protocol.MaxSessionUnprocessedPackets+10; i++ {
			sess.handlePacket(&receivedPacket{})
		}
		close(done)
	}, 0.5)

	Context("getting streams", func() {
		It("returns a new stream", func() {
			mstr := NewMockStreamI(mockCtrl)
			streamManager.EXPECT().GetOrOpenSendStream(protocol.StreamID(11)).Return(mstr, nil)
			str, err := sess.GetOrOpenStream(11)
			Expect(err).ToNot(HaveOccurred())
			Expect(str).To(Equal(mstr))
		})

		It("returns a nil-value (not an interface with value nil) for closed streams", func() {
			strI := Stream(nil)
			streamManager.EXPECT().GetOrOpenSendStream(protocol.StreamID(1337)).Return(strI, nil)
			str, err := sess.GetOrOpenStream(1337)
			Expect(err).ToNot(HaveOccurred())
			// make sure that the returned value is a plain nil, not an Stream with value nil
			_, ok := str.(Stream)
			Expect(ok).To(BeFalse())
		})

		It("errors when trying to get a unidirectional stream", func() {
			streamManager.EXPECT().GetOrOpenSendStream(protocol.StreamID(100)).Return(&sendStream{}, nil)
			_, err := sess.GetOrOpenStream(100)
			Expect(err).To(MatchError("Stream 100 is not a bidirectional stream"))
		})

		It("opens streams", func() {
			mstr := NewMockStreamI(mockCtrl)
			streamManager.EXPECT().OpenStream().Return(mstr, nil)
			str, err := sess.OpenStream()
			Expect(err).ToNot(HaveOccurred())
			Expect(str).To(Equal(mstr))
		})

		It("opens streams synchronously", func() {
			mstr := NewMockStreamI(mockCtrl)
			streamManager.EXPECT().OpenStreamSync().Return(mstr, nil)
			str, err := sess.OpenStreamSync()
			Expect(err).ToNot(HaveOccurred())
			Expect(str).To(Equal(mstr))
		})

		It("opens unidirectional streams", func() {
			mstr := NewMockSendStreamI(mockCtrl)
			streamManager.EXPECT().OpenUniStream().Return(mstr, nil)
			str, err := sess.OpenUniStream()
			Expect(err).ToNot(HaveOccurred())
			Expect(str).To(Equal(mstr))
		})

		It("opens unidirectional streams synchronously", func() {
			mstr := NewMockSendStreamI(mockCtrl)
			streamManager.EXPECT().OpenUniStreamSync().Return(mstr, nil)
			str, err := sess.OpenUniStreamSync()
			Expect(err).ToNot(HaveOccurred())
			Expect(str).To(Equal(mstr))
		})

		It("accepts streams", func() {
			mstr := NewMockStreamI(mockCtrl)
			streamManager.EXPECT().AcceptStream().Return(mstr, nil)
			str, err := sess.AcceptStream()
			Expect(err).ToNot(HaveOccurred())
			Expect(str).To(Equal(mstr))
		})

		It("accepts unidirectional streams", func() {
			mstr := NewMockReceiveStreamI(mockCtrl)
			streamManager.EXPECT().AcceptUniStream().Return(mstr, nil)
			str, err := sess.AcceptUniStream()
			Expect(err).ToNot(HaveOccurred())
			Expect(str).To(Equal(mstr))
		})
	})

	It("returns the local address", func() {
		addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1337}
		mconn.localAddr = addr
		Expect(sess.LocalAddr()).To(Equal(addr))
	})

	It("returns the remote address", func() {
		addr := &net.UDPAddr{IP: net.IPv4(1, 2, 7, 1), Port: 7331}
		mconn.remoteAddr = addr
		Expect(sess.RemoteAddr()).To(Equal(addr))
	})
})

var _ = Describe("Client Session", func() {
	var (
		sess          *session
		sessionRunner *MockSessionRunner
		packer        *MockPacker
		mconn         *mockConnection
		cryptoSetup   *mocks.MockCryptoSetup
	)

	BeforeEach(func() {
		Eventually(areSessionsRunning).Should(BeFalse())

		mconn = newMockConnection()
		sessionRunner = NewMockSessionRunner(mockCtrl)
		sessP, err := newClientSession(
			mconn,
			sessionRunner,
			protocol.ConnectionID{8, 7, 6, 5, 4, 3, 2, 1},
			protocol.ConnectionID{1, 2, 3, 4, 5, 6, 7, 8},
			populateClientConfig(&Config{}, true),
			nil, // tls.Config
			42,  // initial packet number
			&handshake.TransportParameters{},
			protocol.VersionWhatever,
			utils.DefaultLogger,
			protocol.VersionWhatever,
		)
		sess = sessP.(*session)
		Expect(err).ToNot(HaveOccurred())
		packer = NewMockPacker(mockCtrl)
		sess.packer = packer
		cryptoSetup = mocks.NewMockCryptoSetup(mockCtrl)
		sess.cryptoStreamHandler = cryptoSetup
	})

	It("changes the connection ID when receiving the first packet from the server", func() {
		unpacker := NewMockUnpacker(mockCtrl)
		unpacker.EXPECT().Unpack(gomock.Any(), gomock.Any()).DoAndReturn(func(hdr *wire.Header, data []byte) (*unpackedPacket, error) {
			return &unpackedPacket{
				encryptionLevel: protocol.Encryption1RTT,
				hdr:             &wire.ExtendedHeader{Header: *hdr},
				data:            []byte{0}, // one PADDING frame
			}, nil
		})
		sess.unpacker = unpacker
		go func() {
			defer GinkgoRecover()
			cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() }).AnyTimes()
			sess.run()
		}()
		newConnID := protocol.ConnectionID{1, 3, 3, 7, 1, 3, 3, 7}
		packer.EXPECT().ChangeDestConnectionID(newConnID)
		Expect(sess.handlePacketImpl(insertPacketBuffer(&receivedPacket{
			hdr: &wire.Header{
				IsLongHeader:     true,
				Type:             protocol.PacketTypeHandshake,
				SrcConnectionID:  newConnID,
				DestConnectionID: sess.srcConnID,
				Length:           1,
			},
			data: []byte{0},
		}))).To(BeTrue())
		// make sure the go routine returns
		packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
		sessionRunner.EXPECT().retireConnectionID(gomock.Any())
		cryptoSetup.EXPECT().Close()
		Expect(sess.Close()).To(Succeed())
		Eventually(sess.Context().Done()).Should(BeClosed())
	})

	Context("handling Retry", func() {
		var validRetryHdr *wire.Header

		BeforeEach(func() {
			validRetryHdr = &wire.Header{
				IsLongHeader:         true,
				Type:                 protocol.PacketTypeRetry,
				SrcConnectionID:      protocol.ConnectionID{0xde, 0xad, 0xbe, 0xef},
				DestConnectionID:     protocol.ConnectionID{1, 2, 3, 4, 5, 6, 7, 8},
				OrigDestConnectionID: protocol.ConnectionID{8, 7, 6, 5, 4, 3, 2, 1},
				Token:                []byte("foobar"),
			}
		})

		getPacket := func(hdr *wire.Header) *receivedPacket {
			buf := &bytes.Buffer{}
			(&wire.ExtendedHeader{Header: *hdr}).Write(buf, sess.version)
			return &receivedPacket{
				hdr:    hdr,
				data:   buf.Bytes(),
				buffer: getPacketBuffer(),
			}
		}

		It("handles Retry packets", func() {
			cryptoSetup.EXPECT().ChangeConnectionID(protocol.ConnectionID{0xde, 0xad, 0xbe, 0xef})
			packer.EXPECT().SetToken([]byte("foobar"))
			packer.EXPECT().ChangeDestConnectionID(protocol.ConnectionID{0xde, 0xad, 0xbe, 0xef})
			Expect(sess.handlePacketImpl(getPacket(validRetryHdr))).To(BeTrue())
		})

		It("ignores Retry packets after receiving a regular packet", func() {
			sess.receivedFirstPacket = true
			Expect(sess.handlePacketImpl(getPacket(validRetryHdr))).To(BeFalse())
		})

		It("ignores Retry packets if the server didn't change the connection ID", func() {
			validRetryHdr.SrcConnectionID = sess.destConnID
			Expect(sess.handlePacketImpl(getPacket(validRetryHdr))).To(BeFalse())
		})

		It("ignores Retry packets with the wrong original destination connection ID", func() {
			hdr := &wire.Header{
				IsLongHeader:         true,
				Type:                 protocol.PacketTypeRetry,
				SrcConnectionID:      protocol.ConnectionID{0xde, 0xad, 0xbe, 0xef},
				DestConnectionID:     protocol.ConnectionID{1, 2, 3, 4, 5, 6, 7, 8},
				OrigDestConnectionID: protocol.ConnectionID{1, 2, 3, 4},
				Token:                []byte("foobar"),
			}
			Expect(sess.handlePacketImpl(getPacket(hdr))).To(BeFalse())
		})
	})

	Context("transport parameters", func() {
		It("errors if it can't unmarshal the TransportParameters", func() {
			go func() {
				defer GinkgoRecover()
				cryptoSetup.EXPECT().RunHandshake().Do(func() { <-sess.Context().Done() })
				err := sess.run()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("transport parameter"))
			}()
			// streamManager.EXPECT().CloseWithError(gomock.Any())
			sessionRunner.EXPECT().retireConnectionID(gomock.Any())
			packer.EXPECT().PackConnectionClose(gomock.Any()).Return(&packedPacket{}, nil)
			cryptoSetup.EXPECT().Close()
			sess.processTransportParameters([]byte("invalid"))
			Eventually(sess.Context().Done()).Should(BeClosed())
		})

		It("errors if the TransportParameters don't contain the stateless reset token", func() {
			eetp := &handshake.EncryptedExtensionsTransportParameters{
				NegotiatedVersion: sess.version,
				SupportedVersions: []protocol.VersionNumber{sess.version},
			}
			_, err := sess.processTransportParametersForClient(eetp.Marshal())
			Expect(err).To(MatchError("server didn't send stateless_reset_token"))
		})

		It("errors if the TransportParameters contain an original_connection_id, although no Retry was performed", func() {
			eetp := &handshake.EncryptedExtensionsTransportParameters{
				NegotiatedVersion: sess.version,
				SupportedVersions: []protocol.VersionNumber{sess.version},
				Parameters: handshake.TransportParameters{
					OriginalConnectionID: protocol.ConnectionID{0xde, 0xca, 0xfb, 0xad},
					StatelessResetToken:  []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
				},
			}
			_, err := sess.processTransportParametersForClient(eetp.Marshal())
			Expect(err).To(MatchError("expected original_connection_id to equal (empty), is 0xdecafbad"))
		})

		It("errors if the TransportParameters contain an original_connection_id, although no Retry was performed", func() {
			sess.origDestConnID = protocol.ConnectionID{0xde, 0xad, 0xbe, 0xef}
			eetp := &handshake.EncryptedExtensionsTransportParameters{
				NegotiatedVersion: sess.version,
				SupportedVersions: []protocol.VersionNumber{sess.version},
				Parameters: handshake.TransportParameters{
					OriginalConnectionID: protocol.ConnectionID{0xde, 0xca, 0xfb, 0xad},
					StatelessResetToken:  []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
				},
			}
			_, err := sess.processTransportParametersForClient(eetp.Marshal())
			Expect(err).To(MatchError("expected original_connection_id to equal 0xdeadbeef, is 0xdecafbad"))
		})

		Context("Version Negotiation", func() {
			var params handshake.TransportParameters

			BeforeEach(func() {
				params = handshake.TransportParameters{
					StatelessResetToken:  []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
					OriginalConnectionID: sess.origDestConnID,
				}
			})

			It("accepts a valid version negotiation", func() {
				sess.initialVersion = 13
				sess.version = 37
				sess.config.Versions = []protocol.VersionNumber{13, 37, 42}
				eetp := &handshake.EncryptedExtensionsTransportParameters{
					NegotiatedVersion: 37,
					SupportedVersions: []protocol.VersionNumber{36, 37, 38},
					Parameters:        params,
				}
				_, err := sess.processTransportParametersForClient(eetp.Marshal())
				Expect(err).ToNot(HaveOccurred())
			})

			It("errors if the current version doesn't match negotiated_version", func() {
				sess.initialVersion = 13
				sess.version = 37
				sess.config.Versions = []protocol.VersionNumber{13, 37, 42}
				eetp := &handshake.EncryptedExtensionsTransportParameters{
					NegotiatedVersion: 38,
					SupportedVersions: []protocol.VersionNumber{36, 37, 38},
					Parameters:        params,
				}
				_, err := sess.processTransportParametersForClient(eetp.Marshal())
				Expect(err).To(MatchError("VersionNegotiationMismatch: current version doesn't match negotiated_version"))
			})

			It("errors if the current version is not contained in the server's supported versions", func() {
				sess.version = 42
				eetp := &handshake.EncryptedExtensionsTransportParameters{
					NegotiatedVersion: 42,
					SupportedVersions: []protocol.VersionNumber{43, 44},
					Parameters:        params,
				}
				_, err := sess.processTransportParametersForClient(eetp.Marshal())
				Expect(err).To(MatchError("VersionNegotiationMismatch: current version not included in the supported versions"))
			})

			It("errors if version negotiation was performed, but would have picked a different version based on the supported version list", func() {
				sess.version = 42
				sess.initialVersion = 41
				sess.config.Versions = []protocol.VersionNumber{43, 42, 41}
				serverSupportedVersions := []protocol.VersionNumber{42, 43}
				// check that version negotiation would have led us to pick version 43
				ver, ok := protocol.ChooseSupportedVersion(sess.config.Versions, serverSupportedVersions)
				Expect(ok).To(BeTrue())
				Expect(ver).To(Equal(protocol.VersionNumber(43)))
				eetp := &handshake.EncryptedExtensionsTransportParameters{
					NegotiatedVersion: 42,
					SupportedVersions: serverSupportedVersions,
					Parameters:        params,
				}
				_, err := sess.processTransportParametersForClient(eetp.Marshal())
				Expect(err).To(MatchError("VersionNegotiationMismatch: would have picked a different version"))
			})

			It("doesn't error if it would have picked a different version based on the supported version list, if no version negotiation was performed", func() {
				sess.version = 42
				sess.initialVersion = 42 // version == initialVersion means no version negotiation was performed
				sess.config.Versions = []protocol.VersionNumber{43, 42, 41}
				serverSupportedVersions := []protocol.VersionNumber{42, 43}
				// check that version negotiation would have led us to pick version 43
				ver, ok := protocol.ChooseSupportedVersion(sess.config.Versions, serverSupportedVersions)
				Expect(ok).To(BeTrue())
				Expect(ver).To(Equal(protocol.VersionNumber(43)))
				eetp := &handshake.EncryptedExtensionsTransportParameters{
					NegotiatedVersion: 42,
					SupportedVersions: serverSupportedVersions,
					Parameters:        params,
				}
				_, err := sess.processTransportParametersForClient(eetp.Marshal())
				Expect(err).ToNot(HaveOccurred())
			})
		})

	})
})
