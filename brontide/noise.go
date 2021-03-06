package brontide

import (
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"io"

	"golang.org/x/crypto/hkdf"

	"github.com/aead/chacha20"
	"github.com/roasbeef/btcd/btcec"
)

const (
	// protocolName is the precise instantiation of the Noise protocol
	// handshake at the center of Brontide. This value will be used as part
	// of the prologue. If the initiator and responder aren't using the
	// exact same string for this value, along with prologue of the Bitcoin
	// network, then the initial handshake will fail.
	protocolName = "Noise_XK_secp256k1_ChaChaPoly_SHA256"
)

// TODO(roasbeef): free buffer pool?

// cipherState encapsulates the state for the AEAD which will be used to
// encrypt+authenticate any payloads sent during the handshake, and messages
// sent once the handshake has completed.
type cipherState struct {
	// nonce is the nonce passed into the chacha20-poly1305 instance for
	// encryption+decryption. The nonce is incremented after each succesful
	// encryption/decryption.
	//
	// TODO(roasbeef): this should actually be 96 bit
	nonce uint64

	// secretKey is the shared symmetric key which will be used to
	// instantiate the cipher.
	//
	// TODO(roasbeef): m-lock??
	secretKey [32]byte

	// cipher is an instance of the ChaCha20-Poly1305 AEAD construction
	// created using the secretKey above.
	cipher cipher.AEAD
}

// Encrypt returns a ciphertext which is the encryption of the plainText
// observing the passed associatedData within the AEAD construction.
func (c *cipherState) Encrypt(associatedData, cipherText, plainText []byte) []byte {
	defer func() {
		c.nonce++
	}()

	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[:], c.nonce)

	return c.cipher.Seal(cipherText, nonce[:], plainText, associatedData)
}

// Decrypt attempts to decrypt the passed ciphertext observing the specified
// associatedData within the AEAD construction. In the case that the final MAC
// check fails, then a non-nil error will be returned.
func (c *cipherState) Decrypt(associatedData, plainText, cipherText []byte) ([]byte, error) {
	defer func() {
		c.nonce++
	}()

	var nonce [12]byte
	binary.LittleEndian.PutUint64(nonce[:], c.nonce)

	return c.cipher.Open(plainText, nonce[:], cipherText, associatedData)
}

// InitializeKey initializes the secret key and AEAD cipher scheme based off of
// the passed key.
func (c *cipherState) InitializeKey(key [32]byte) {
	c.secretKey = key
	c.nonce = 0
	c.cipher = chacha20.NewChaCha20Poly1305(&c.secretKey)
}

// symmetricState encapsulates a cipherState object and houses the ephemeral
// handshake digest state. This struct is used during the handshake to derive
// new shared secrets based off of the result of ECDH operations. Ultimately,
// the final key yielded by this struct is the result of an incremental
// Triple-DH operation.
type symmetricState struct {
	cipherState

	// chainingKey is used as the salt to the HKDF function to derive a new
	// chaining key as well as a new tempKey which is used for
	// encryption/decryption.
	chainingKey [32]byte

	// tempKey is the latter 32 bytes resulted from the latest HKDF
	// iteration. This key is used to encrypt/decrypt any handshake
	// messages or payloads sent until the next DH operation is executed.
	tempKey [32]byte

	// handshakeDigest is the cummulative hash digest of all handshake
	// messages sent from start to finish. This value is never transmitted
	// to the other side, but will be used as the AD when
	// encrypting/decrypting messages using our AEAD construction.
	handshakeDigest [32]byte
}

// mixKey is implements a basic HKDF-based key rachet. This method is called
// with the result of each DH output generated during the handshake process.
// The first 32 bytes extract from the HKDF reader is the next chaining key,
// then latter 32 bytes become the temp secret key using within any future AEAD
// operations until another DH operation is performed.
func (s *symmetricState) mixKey(input []byte) {
	var info []byte

	secret := input
	salt := s.chainingKey
	h := hkdf.New(sha256.New, secret, salt[:], info)

	// hkdf(input, ck, zero)
	// |
	// | \
	// |  \
	// ck  k
	h.Read(s.chainingKey[:])
	h.Read(s.tempKey[:])

	// cipher.k = temp_key
	s.InitializeKey(s.tempKey)
}

// mixHash hashes the passed input data into the cummulative handshake digest.
// The running result of this value (h) is used as the associated data in all
// decryption/encryption operations.
func (s *symmetricState) mixHash(data []byte) {
	h := sha256.New()
	h.Write(s.handshakeDigest[:])
	h.Write(data)

	copy(s.handshakeDigest[:], h.Sum(nil))
}

// EncryptAndHash returns the authenticated encryption of the passed plaintext.
// When encrypting the handshake digest (h) is used as the associated data to
// the AEAD cipher.
func (s *symmetricState) EncryptAndHash(plaintext []byte) []byte {
	ciphertext := s.Encrypt(s.handshakeDigest[:], nil, plaintext)

	s.mixHash(ciphertext)

	return ciphertext
}

// DecryptAndHash returns the authenticated decryption of the passed
// ciphertext.  When encrypting the handshake digest (h) is used as the
// associated data to the AEAD cipher.
func (s *symmetricState) DecryptAndHash(ciphertext []byte) ([]byte, error) {
	plaintext, err := s.Decrypt(s.handshakeDigest[:], nil, ciphertext)
	if err != nil {
		return nil, err
	}

	s.mixHash(ciphertext)

	return plaintext, nil
}

// InitializeSymmetric initializes the symmetric state by setting the handshake
// digest (h) and the chaining key (ck) to protocol name.
func (s *symmetricState) InitializeSymmetric(protocolName []byte) {
	var empty [32]byte

	s.handshakeDigest = sha256.Sum256(protocolName)
	s.chainingKey = s.handshakeDigest
	s.InitializeKey(empty)
}

// handshakeState encapsulates the symmetricState and keeps track of all the
// public keys (static and ephemeral) for both sides during the handshake
// transscript. If the handshake completes successfuly, then two instances of a
// cipherState are emitted: one to encrypt messages from initiator to
// responder, and the other for the opposite direction.
type handshakeState struct {
	symmetricState

	initiator bool

	localStatic    *btcec.PrivateKey
	localEphemeral *btcec.PrivateKey

	remoteStatic    *btcec.PublicKey
	remoteEphemeral *btcec.PublicKey
}

// newHandshakeState returns a new instance of the handshake state initialized
// with the prologue and protocol name. If this is the respodner's handshake
// state, then the remotePub can be nil.
func newHandshakeState(initiator bool, prologue []byte,
	localPub *btcec.PrivateKey, remotePub *btcec.PublicKey) handshakeState {

	h := handshakeState{
		initiator:    initiator,
		localStatic:  localPub,
		remoteStatic: remotePub,
	}

	// Set the current chainking key and handshake digest to the hash of
	// the protocol name, and additionally mix in the prologue. If either
	// sides disagree about the prologue or protocol name, then the
	// handshake will fail.
	h.InitializeSymmetric([]byte(protocolName))
	h.mixHash(prologue)

	// In Noise_XK, then initiator should know the responder's static
	// public key, therefore we include the responder's static key in the
	// handshake digest. If the initiator gets this value wrong, then the
	// handshake will fail.
	if initiator {
		h.mixHash(remotePub.SerializeCompressed())
	} else {
		h.mixHash(localPub.PubKey().SerializeCompressed())
	}

	return h
}

// BrontideMachine is a state-machine which implements Brontide: an
// Authenticated-key Exchange in Three Acts. Brontide is derived from the Noise
// framework, specifically implementing the Noise_XK handshake. Once the
// initial 3-act handshake has completed all messages are encrypted with a
// chacha20 AEAD cipher. On the wire, all messages are prefixed with an
// authenticated+encrypted length field. Additionally, the encrypted+auth'd
// length prefix is used as the AD when encrypting+decryption messages. This
// construction provides confidentiallity of packet length, avoids introducing
// a padding-oracle, and binds the encrypted packet length to the packet
// itself.
//
// The acts proceeds the following order (initiator on the left):
//  GenActOne()   ->
//                    RecvActOne()
//                <-  GenActTwo()
//  RecvActTwo()
//  GenActThree() ->
//                    RecvActThree()
//
// This exchange corresponds to the following Noise handshake:
//   <- s
//   ...
//   -> e, es
//   <- e, ee
//   -> s, se
type BrontideMachine struct {
	sendCipher cipherState
	recvCipher cipherState

	handshakeState
}

// NewBrontideMachine creates a new instance of the brontide state-machine. If
// the responder (listener) is creating the object, then the remotePub should
// be nil. The handshake state within brontide is initialized using the ascii
// string "bitcoin" as the prologue.
func NewBrontideMachine(initiator bool, localPub *btcec.PrivateKey,
	remotePub *btcec.PublicKey) *BrontideMachine {

	handshake := newHandshakeState(initiator, []byte("bitcoin"), localPub,
		remotePub)

	return &BrontideMachine{handshakeState: handshake}
}

const (
	// ActOneSize is the size of the packet sent from initiator to
	// responder in ActOne. The packet consists of an ephemeral key in
	// compressed format, and a 16-byte poly1305 tag.
	//
	// 33 + 16
	ActOneSize = 49

	// ActTwoSize is the size the packet sent from responder to initiator
	// in ActTwo. The packet consists of an ephemeral key in compressed
	// format and a 16-byte poly1305 tag.
	//
	// 33 + 16
	ActTwoSize = 49

	// ActThreeSize is the size of the packet sent from initiator to
	// responder in ActThree. The packet consists of the initiators static
	// key encrypted with strong forward secrecy and a 16-byte poly1035
	// tag.
	//
	// 33 + 16 + 16
	ActThreeSize = 65
)

// GenActOne generates the initial packet (act one) to be sent from initiator
// to responder. During act one the initiator generates a fresh ephemeral key,
// hashes it into the handshake digest, and performs an ECDH between this key
// and the responder's static key. Future payloads are encrypted with a key
// dervied from this result.
//
//    -> e, es
func (b *BrontideMachine) GenActOne() ([ActOneSize]byte, error) {
	var (
		err    error
		actOne [ActOneSize]byte
	)

	// e
	b.localEphemeral, err = btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return actOne, err
	}

	ephemeral := b.localEphemeral.PubKey().SerializeCompressed()
	b.mixHash(ephemeral)

	// es
	s := btcec.GenerateSharedSecret(b.localEphemeral, b.remoteStatic)
	b.mixKey(s)

	authPayload := b.EncryptAndHash([]byte{})

	copy(actOne[:33], ephemeral)
	copy(actOne[33:], authPayload)

	return actOne, nil
}

// RecvActOne processes the act one packet sent by the initiator. The responder
// executes the mirroed actions to that of the initiator extending the
// handshake digest and deriving a new shared secret based on a ECDH with the
// initiator's ephemeral key and reponder's static key.
func (b *BrontideMachine) RecvActOne(actOne [ActOneSize]byte) error {
	var (
		err error
		e   [33]byte
		p   [16]byte
	)

	copy(e[:], actOne[:33])
	copy(p[:], actOne[33:])

	// e
	b.remoteEphemeral, err = btcec.ParsePubKey(e[:], btcec.S256())
	if err != nil {
		return err
	}
	b.mixHash(b.remoteEphemeral.SerializeCompressed())

	// es
	s := btcec.GenerateSharedSecret(b.localStatic, b.remoteEphemeral)
	b.mixKey(s)

	// If the initiator doesn't know our static key, then this operation
	// will fail.
	if _, err := b.DecryptAndHash(p[:]); err != nil {
		return err
	}

	return nil
}

// GenActTwo generates the second packet (act two) to be sent from the
// responder to the initiator. The packet for act two is identify to that of
// act one, but then results in a different ECDH operation between the
// initiator's and responder's ephemeral keys.
//
//    <- e, ee
func (b *BrontideMachine) GenActTwo() ([ActTwoSize]byte, error) {
	var (
		err    error
		actTwo [ActTwoSize]byte
	)

	// e
	b.localEphemeral, err = btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return actTwo, err
	}

	ephemeral := b.localEphemeral.PubKey().SerializeCompressed()
	b.mixHash(b.localEphemeral.PubKey().SerializeCompressed())

	// ee
	s := btcec.GenerateSharedSecret(b.localEphemeral, b.remoteEphemeral)
	b.mixKey(s)

	authPayload := b.EncryptAndHash([]byte{})

	copy(actTwo[:33], ephemeral)
	copy(actTwo[33:], authPayload)

	return actTwo, nil
}

// RecvActTwo processes the second packet (act two) sent from the responder to
// the initiator. A succesful processing of this packet authenticates the
// initiator to the responder.
func (b *BrontideMachine) RecvActTwo(actTwo [ActTwoSize]byte) error {
	var (
		err error
		e   [33]byte
		p   [16]byte
	)

	copy(e[:], actTwo[:33])
	copy(p[:], actTwo[33:])

	// e
	b.remoteEphemeral, err = btcec.ParsePubKey(e[:], btcec.S256())
	if err != nil {
		return err
	}
	b.mixHash(b.remoteEphemeral.SerializeCompressed())

	// ee
	s := btcec.GenerateSharedSecret(b.localEphemeral, b.remoteEphemeral)
	b.mixKey(s)

	if _, err := b.DecryptAndHash(p[:]); err != nil {
		return err
	}

	return nil
}

// GenActThree creates the final (act three) packet of the handshake. Act three
// is to be sent from the initiator to the responder. The purpose of act three
// is to transmit the initiator's public key under strong forwad secrecy to the
// responder. This act also includes the final ECDH operation which yields the
// final session.
//
//    -> s, se
func (b *BrontideMachine) GenActThree() ([ActThreeSize]byte, error) {
	var actThree [ActThreeSize]byte

	ourPubkey := b.localStatic.PubKey().SerializeCompressed()
	ciphertext := b.EncryptAndHash(ourPubkey)

	s := btcec.GenerateSharedSecret(b.localStatic, b.remoteEphemeral)
	b.mixKey(s)

	authPayload := b.EncryptAndHash([]byte{})

	copy(actThree[:49], ciphertext)
	copy(actThree[49:], authPayload)

	// With the final ECDH operation complete, derive the session sending
	// and receiving keys.
	b.split()

	return actThree, nil
}

// RecvActThree processes the final act (act three) sent from the initiator to
// the responder. After processing this act, the responder learns of the
// initiators's static public key. Decryption of the static key serves to
// authenticate the initiator to the responder.
func (b *BrontideMachine) RecvActThree(actThree [ActThreeSize]byte) error {
	var (
		err error
		s   [33 + 16]byte
		p   [16]byte
	)

	copy(s[:], actThree[:33+16])
	copy(p[:], actThree[33+16:])

	// s
	remotePub, err := b.DecryptAndHash(s[:])
	if err != nil {
		return err
	}
	b.remoteStatic, err = btcec.ParsePubKey(remotePub, btcec.S256())
	if err != nil {
		return err
	}

	// se
	se := btcec.GenerateSharedSecret(b.localEphemeral, b.remoteStatic)
	b.mixKey(se)

	if _, err := b.DecryptAndHash(p[:]); err != nil {
		return err
	}

	// With the final ECDH operation complete, derive the session sending
	// and receiving keys.
	b.split()

	return nil
}

// split is the final wrap-up act to be executed at the end of a succesful
// three act handshake. This function creates to internal cipherState
// instances: one which is used to encrypt messages from the initiator to the
// responder, and another which is used to encrypt message for the opposite
// direction.
func (b *BrontideMachine) split() {
	var (
		empty   []byte
		sendKey [32]byte
		recvKey [32]byte
	)

	h := hkdf.New(sha256.New, b.chainingKey[:], empty, empty)

	// If we're the initiator the the frist 32 bytes are used to encrypt
	// our messages and the second 32-bytes to decrypt their messages. For
	// the responder the opposite is true.
	if b.initiator {
		h.Read(sendKey[:])
		b.sendCipher = cipherState{}
		b.sendCipher.InitializeKey(sendKey)

		h.Read(recvKey[:])
		b.recvCipher = cipherState{}
		b.recvCipher.InitializeKey(recvKey)
	} else {
		h.Read(recvKey[:])
		b.recvCipher = cipherState{}
		b.recvCipher.InitializeKey(recvKey)

		h.Read(sendKey[:])
		b.sendCipher = cipherState{}
		b.sendCipher.InitializeKey(sendKey)
	}
}

// WriteMessage writes the next message p to the passed io.Writer. The
// ciphertext of the message is pre-pended with an encrypt+auth'd length which
// must be used as the AD to the AEAD construction when being decrypted by the
// other side.
func (b *BrontideMachine) WriteMessage(w io.Writer, p []byte) error {
	// The full length of the packet includes the 16 byte MAC.
	fullLength := uint64(len(p) + 16)

	// TODO(roasbeef): The Summit decided on 24 bits?
	var pktLen [8]byte
	binary.BigEndian.PutUint64(pktLen[:], fullLength)

	// First, write out the encrypted+MAC'd length prefix for the packet.
	cipherLen := b.sendCipher.Encrypt(nil, nil, pktLen[:])
	if _, err := w.Write(cipherLen); err != nil {
		return err
	}

	// Next, write out the encrypted packet itself. We use the encrypted
	// packet length above as the AD to the cipher in order to bind both
	// messages together thwarting an active attack.
	cipherText := b.sendCipher.Encrypt(cipherLen, nil, p)
	if _, err := w.Write(cipherText); err != nil {
		return err
	}

	return nil
}

// ReadMessage attemps to read the next message from the passed io.Reader. In
// the case of an authentication error, a non-nil error is returned.
func (b *BrontideMachine) ReadMessage(r io.Reader) ([]byte, error) {
	var cipherLen [8 + 16]byte
	if _, err := io.ReadFull(r, cipherLen[:]); err != nil {
		return nil, err
	}

	// Attempt to decrypt+auth the packet length present in the stream.
	pktLenBytes, err := b.recvCipher.Decrypt(nil, nil, cipherLen[:])
	if err != nil {
		return nil, err
	}

	// Next, using the length read from the packet header, read the
	// encrypted packet itself.
	pktLen := binary.BigEndian.Uint64(pktLenBytes)
	ciperText := make([]byte, pktLen)
	if _, err := io.ReadFull(r, ciperText[:]); err != nil {
		return nil, err
	}

	// Finally, return the decrypted packet ensuring that the encrypted
	// packet length is authenticated along with the packet itself.
	return b.recvCipher.Decrypt(cipherLen[:], nil, ciperText)
}

// TODO(roasbeef): key rotation
