package stl

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"math/rand"
	"net"
	"sync"
	"testing"

	"github.com/DavidHuie/signcryption"
	"github.com/ethereum/go-ethereum/crypto/ecies"
)

func TestConnIntegration(t *testing.T) {
	r := rand.New(rand.NewSource(0))
	clientID := getRandBytes(r, 16)
	clientPriv, err := ecdsa.GenerateKey(elliptic.P256(), r)
	if err != nil {
		t.Fatal(err)
	}
	serverID := getRandBytes(r, 16)
	serverPriv, err := ecdsa.GenerateKey(elliptic.P256(), r)
	if err != nil {
		t.Fatal(err)
	}
	tunnelID := getRandBytes(r, 16)
	tunnelPriv, err := ecdsa.GenerateKey(elliptic.P256(), r)
	if err != nil {
		t.Fatal(err)
	}

	verifier := &sessionVerifierImpl{
		clientID:  clientID,
		clientPub: ecies.ImportECDSAPublic(&clientPriv.PublicKey),
		tunnelID:  tunnelID,
		tunnelPub: ecies.ImportECDSAPublic(&tunnelPriv.PublicKey),
	}

	listener, err := net.Listen("tcp", ":")
	if err != nil {
		t.Fatal(err)
	}

	var serverSessionKey []byte

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()

		serverConn, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}

		conn := NewServerConn(serverConn, &ServerConfig{
			ID:                   serverID,
			SignaturePrivateKey:  serverPriv,
			EncryptionPrivateKey: signcryption.PrivateKeyFromECDSA(serverPriv),
			SessionVerifier:      verifier,
		})
		if err := conn.handshakeAsServer(); err != nil {
			t.Error(err)
		}

		serverSessionKey = conn.sessionKey
	}()

	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn := NewConn(clientConn, &ClientConfig{
		ClientID:                  clientID,
		HandshakePrivateKey:       ecies.ImportECDSA(clientPriv),
		ServerHandshakePublicKey:  &serverPriv.PublicKey,
		ServerID:                  serverID,
		ServerEncryptionPublicKey: signcryption.PublicKeyFromECDSA(&serverPriv.PublicKey),
		TunnelEncryptionPublicKey: &tunnelPriv.PublicKey,
		TunnelID:                  tunnelID,
		EncryptionPrivateKey:      signcryption.PrivateKeyFromECDSA(clientPriv),
	})

	if err := conn.handshakeAsClient(); err != nil {
		t.Fatal(err)
	}

	listener.Close()
	wg.Wait()

	if bytes.Compare(conn.sessionKey, serverSessionKey) != 0 {
		t.Fatal("session keys must match")
	}
}
