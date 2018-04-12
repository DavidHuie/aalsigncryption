package stl

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"

	"github.com/DavidHuie/signcryption"
	"github.com/DavidHuie/signcryption/aal"
	"github.com/ethereum/go-ethereum/crypto/ecies"
	"github.com/pkg/errors"
	"github.com/vmihailenco/msgpack"
)

const (
	// This is ripped stolen from the TLS code. This value should
	// reflect the encryption that's in use here and requires some
	// tuning.
	// TODO: tune
	maxPlaintext = 16384
)

type SessionVerifier interface {
	VerifySession([]byte, *ecies.PublicKey, *ecdsa.PublicKey, []byte, *ecies.PublicKey) (bool, error)
}

type ClientConfig struct {
	ClientID                  []byte
	HandshakePrivateKey       *ecies.PrivateKey
	EncryptionPrivateKey      *signcryption.PrivateKey
	ServerEncryptionPublicKey *signcryption.PublicKey
	ServerHandshakePublicKey  *ecdsa.PublicKey
	ServerID                  []byte
	TunnelEncryptionPublicKey *ecdsa.PublicKey
	TunnelID                  []byte
}

type ServerConfig struct {
	ID                   []byte
	SignaturePrivateKey  *ecdsa.PrivateKey
	EncryptionPrivateKey *signcryption.PrivateKey
	SessionVerifier      SessionVerifier
}

type Conn struct {
	conn            net.Conn
	clientConfig    *ClientConfig
	serverConfig    *ServerConfig
	sessionKey      []byte
	publicKey       *signcryption.PublicKey
	privateKey      *signcryption.PrivateKey
	aal             aal.AAL
	readBuf         *bytes.Buffer
	writtenSegments uint64
	readSegments    uint64
}

func NewConn(c net.Conn, config *ClientConfig) *Conn {
	return &Conn{
		conn:         c,
		clientConfig: config,
		publicKey:    config.ServerEncryptionPublicKey,
		privateKey:   config.EncryptionPrivateKey,
		aal:          aal.NewP256(),
		readBuf:      &bytes.Buffer{},
	}
}

func NewServerConn(c net.Conn, config *ServerConfig) *Conn {
	return &Conn{
		conn:         c,
		serverConfig: config,
		readBuf:      &bytes.Buffer{},
		privateKey:   config.EncryptionPrivateKey,
		aal:          aal.NewP256(),
	}
}

func (c *Conn) Handshake() error {
	if len(c.sessionKey) != 0 {
		return nil
	}
	if c.clientConfig != nil {
		return c.handshakeAsClient()
	}
	return c.handshakeAsServer()
}

func (c *Conn) handshakeAsServer() error {
	// Read in request
	requestSizeBytes := make([]byte, 8)
	if _, err := io.ReadFull(c.conn, requestSizeBytes); err != nil {
		return errors.Wrapf(err, "error reading handshake request size")
	}
	requestSize := binary.LittleEndian.Uint64(requestSizeBytes)
	requestBytes := make([]byte, requestSize)
	if _, err := io.ReadFull(c.conn, requestBytes); err != nil {
		return errors.Wrapf(err, "error reading handshake request bytes")
	}
	var request *handshakeRequest
	if err := msgpack.Unmarshal(requestBytes, &request); err != nil {
		return errors.Wrapf(err, "error unmarshaling handshake request")
	}

	// Process request
	handshaker := &serverHandshaker{
		rand:            rand.Reader,
		id:              c.serverConfig.ID,
		priv:            c.serverConfig.SignaturePrivateKey,
		sessionVerifier: c.serverConfig.SessionVerifier,
	}
	response, valid, err := handshaker.processRequest(request)
	if err != nil {
		return errors.Wrapf(err, "error processing handshake request")
	}
	if !valid {
		return errors.New("error: handshake request is not valid")
	}

	// Write response
	responseBytes, err := msgpack.Marshal(response)
	if err != nil {
		return errors.Wrapf(err, "error marshaling handshake response")
	}
	numResponseBytes := len(responseBytes)
	responseBuf := make([]byte, 8+numResponseBytes)
	binary.LittleEndian.PutUint64(responseBuf, uint64(numResponseBytes))
	copy(responseBuf[8:], responseBytes)
	if _, err := io.Copy(c.conn, bytes.NewBuffer(responseBuf)); err != nil {
		return errors.Wrapf(err, "error writing handshake response")
	}

	c.publicKey = signcryption.PublicKeyFromECDSA(response.encryptionPublicKey,
		request.ID)
	c.sessionKey = handshaker.sessionKey

	return nil
}

func (c *Conn) handshakeAsClient() error {
	handshaker := &clientHandshaker{
		rand:           rand.Reader,
		id:             c.clientConfig.ClientID,
		priv:           c.clientConfig.HandshakePrivateKey,
		encryptionPriv: c.clientConfig.EncryptionPrivateKey.ToECDSA(),
		serverPub:      c.clientConfig.ServerHandshakePublicKey,
		serverID:       c.clientConfig.ServerID,
		tunnelPub:      c.clientConfig.TunnelEncryptionPublicKey,
		tunnelID:       c.clientConfig.TunnelID,
	}
	request := handshaker.generateRequest()
	requestBytes, err := msgpack.Marshal(request)
	if err != nil {
		return errors.Wrapf(err, "error marshaling client handshake request")
	}
	numRequestBytes := len(requestBytes)

	handshakeBuf := make([]byte, 8+numRequestBytes)
	binary.LittleEndian.PutUint64(handshakeBuf, uint64(numRequestBytes))
	copy(handshakeBuf[8:], requestBytes)

	// Write request to connection
	if _, err := io.Copy(c.conn, bytes.NewBuffer(handshakeBuf)); err != nil {
		return errors.Wrapf(err, "error writing client handshake request bytes to conn")
	}

	// Read response from connection
	responseSizeBytes := make([]byte, 8)
	if _, err := io.ReadFull(c.conn, responseSizeBytes); err != nil {
		return errors.Wrapf(err, "error reading handshake response size")
	}
	responseSize := binary.LittleEndian.Uint64(responseSizeBytes)
	responseBytes := make([]byte, responseSize)
	if _, err := io.ReadFull(c.conn, responseBytes); err != nil {
		return errors.Wrapf(err, "error reading handshake response bytes")
	}
	var response *handshakeResponse
	if err := msgpack.Unmarshal(responseBytes, &response); err != nil {
		return errors.Wrapf(err, "error unmarshaling handshake response")
	}

	// Validate response
	valid, err := handshaker.processServerResponse(response)
	if err != nil {
		return errors.Wrapf(err, "error processing server handshake response")
	}
	if !valid {
		return errors.New("error: invalid server response")
	}

	c.sessionKey = handshaker.sessionKey

	return nil
}

func (c *Conn) Write(b []byte) (int, error) {
	written := 0

	for i := 0; i < len(b); i += maxPlaintext {
		writeSize := len(b) - written
		if writeSize > maxPlaintext {
			writeSize = maxPlaintext
		}

		if err := c.writeSegment(b[written : written+writeSize]); err != nil {
			return written, errors.Wrapf(err, "error writing segment")
		}

		written += writeSize
	}

	return written, nil
}

func (c *Conn) writeSegment(b []byte) error {
	additionalData := make([]byte, len(c.sessionKey)+8)
	copy(additionalData, c.sessionKey)
	binary.LittleEndian.PutUint64(additionalData[len(c.sessionKey):], c.writtenSegments)

	output, err := c.aal.Signcrypt(c.privateKey, c.publicKey, b, additionalData)
	if err != nil {
		return errors.Wrapf(err, "error signcrypting segment")
	}
	outputBytes, err := msgpack.Marshal(output)
	if err != nil {
		return errors.Wrapf(err, "error marshaling write segment")
	}
	numBytes := len(outputBytes)
	numBytesBytes := make([]byte, 8)

	binary.LittleEndian.PutUint64(numBytesBytes, uint64(numBytes))

	buf := bytes.NewBuffer(numBytesBytes)
	buf.Write(outputBytes)

	if _, err := io.Copy(c.conn, buf); err != nil {
		return errors.Wrapf(err, "error writing segment to connection")
	}

	c.writtenSegments++

	return nil
}

func (c *Conn) Read(b []byte) (int, error) {
	if c.readBuf.Len() == 0 {
		for {
			if err := c.readSegment(); err != nil {
				return 0, errors.Wrapf(err, "error reading segment")
			}

			// TODO: perhaps read more here
			if c.readBuf.Len() >= 0 {
				break
			}
		}
	}

	return c.readBuf.Read(b)
}

func (c *Conn) readSegment() error {
	additionalData := make([]byte, len(c.sessionKey)+8)
	copy(additionalData, c.sessionKey)
	binary.LittleEndian.PutUint64(additionalData[len(c.sessionKey):], c.readSegments)

	numBytesBytes := make([]byte, 8)
	if _, err := io.ReadFull(c.conn, numBytesBytes); err != nil {
		return errors.Wrapf(err, "error reading segment num bytes")
	}
	numBytes := binary.LittleEndian.Uint64(numBytesBytes)

	segmentBytes := make([]byte, numBytes)
	if _, err := io.ReadFull(c.conn, segmentBytes); err != nil {
		return errors.Wrapf(err, "error reading segment bytes")
	}

	var segment *aal.SigncryptionOutput
	if err := msgpack.Unmarshal(segmentBytes, &segment); err != nil {
		return errors.Wrapf(err, "error unmarshaling segment")
	}

	pt, valid, err := c.aal.Unsigncrypt(c.publicKey,
		c.privateKey, additionalData, segment)
	if err != nil {
		return errors.Wrapf(err, "error unsigncrypting segment")
	}
	if !valid {
		return errors.Errorf("error: segment is invalid")
	}

	c.readBuf.Write(pt)
	c.readSegments++

	return nil
}
