// Package tecdsa contains the code that implements Threshold ECDSA signatures.
// The approach is based on [GGN 16].
//
//     [GGN 16]: Gennaro R., Goldfeder S., Narayanan A. (2016) Threshold-Optimal
//          DSA/ECDSA Signatures and an Application to Bitcoin Wallet Security.
//          In: Manulis M., Sadeghi AR., Schneider S. (eds) Applied Cryptography
//          and Network Security. ACNS 2016. Lecture Notes in Computer Science,
//          vol 9696. Springer, Cham
package tecdsa

import (
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"

	"math/big"
	mathrand "math/rand"

	"github.com/keep-network/keep-core/pkg/tecdsa/commitment"
	"github.com/keep-network/keep-core/pkg/tecdsa/curve"
	"github.com/keep-network/keep-core/pkg/tecdsa/zkp"
	"github.com/keep-network/paillier"
)

// PublicParameters for T-ECDSA. Defines how many Signers are in the group,
// what is a group signing threshold, which curve is used and what's the bit
// length of Paillier key.
//
// If we consider an honest-but-curious adversary, i.e. an adversary that learns
// all the secret data of compromised server but does not change their code,
// then [GGN 16] protocol produces signature with n = t + 1 players in the
// network (since all players will behave honestly, even the corrupted ones).
// But in the presence of a malicious adversary, who can force corrupted players
// to shut down or send incorrect messages, one needs at least n = 2t + 1
// players in total to guarantee robustness, i.e. the ability to generate
// signatures even in the presence of malicious faults.
//
// Threshold is just for signing. If anything goes wrong during key generation,
// e.g. one of ZKP fails or any commitment opens incorrectly, key generation
// protocol terminates without an output.
//
// The Curve specified in the PublicParameters is the one used for signing and
// all intermediate constructions during initialization and signing process.
//
// In order for the [GGN 16] protocol to be correct, all the homomorphic
// operations over the ciphertexts (which are modulo N) must not conflict with
// the operations modulo q of the DSA algorithms. Because of that, [GGN 16]
// requires that `N > q^8`, where `N` is a paillier modulus from a Paillier
// publicnkey and `q` is the elliptic curve cardinality.
//
// For instance, secp256k1 cardinality `q`` is a 256 bit number, so we must have
// at least 2048 bit Paillier modulus (Paillier public key).
type PublicParameters struct {
	GroupSize int
	Threshold int

	Curve                elliptic.Curve
	PaillierKeyBitLength int
}

type signerCore struct {
	ID string

	paillierKey *paillier.ThresholdPrivateKey

	groupParameters *PublicParameters
	zkpParameters   *zkp.PublicParameters
}

// LocalSigner represents T-ECDSA group member during the initialization
// phase. It is responsible for constructing a broadcast
// PublicKeyShareCommitmentMessage containing public DSA key share commitment
// and a KeyShareRevealMessage revealing in a Paillier-encrypted way generated
// secret DSA key share and an unencrypted public key share.
type LocalSigner struct {
	signerCore

	dsaKeyShare *dsaKeyShare

	// Intermediate value stored between first and second round of
	// key generation. In the first round, `LocalSigner` commits to the chosen
	// public key share. In the second round, it reveals the public key share
	// along with the decommitment key.
	publicDsaKeyShareDecommitmentKey *commitment.DecommitmentKey
}

// Signer represents T-ECDSA group member in a fully initialized state,
// ready for signing. Each Signer has a reference to a ThresholdDsaKey used
// in a signing process. It represents a (t, n) threshold sharing of the
// underlying DSA key.
type Signer struct {
	signerCore

	dsaKey *ThresholdDsaKey
}

func (pp *PublicParameters) curveCardinality() *big.Int {
	return pp.Curve.Params().N
}

// PublicEcdsaKey returns the group public ECDSA key. This value is the same for
// all signers in the group.
func (s *Signer) PublicEcdsaKey() *curve.Point {
	return s.dsaKey.publicKey
}

// generateDsaKeyShare generates a DSA public and secret key shares and puts
// them into `dsaKeyShare`. Secret key share is a random integer from Z_q where
// `q` is the cardinality of Elliptic Curve and public key share is a point
// on the Curve g^secretKeyShare.
func (ls *LocalSigner) generateDsaKeyShare() (*dsaKeyShare, error) {
	curveParams := ls.groupParameters.Curve.Params()

	secretKeyShare, err := rand.Int(rand.Reader, curveParams.N)
	if err != nil {
		return nil, fmt.Errorf("could not generate DSA key share [%v]", err)
	}

	publicKeyShare := curve.NewPoint(
		ls.groupParameters.Curve.ScalarBaseMult(secretKeyShare.Bytes()),
	)

	return &dsaKeyShare{
		secretKeyShare: secretKeyShare,
		publicKeyShare: publicKeyShare,
	}, nil
}

// InitializeDsaKeyShares initializes key generation process by generating DSA
// key shares and publishing PublicKeyShareCommitmentMessage which is
// broadcasted to all other `Signer`s in the group and contains signer's public
// DSA key share commitment.
func (ls *LocalSigner) InitializeDsaKeyShares() (
	*PublicKeyShareCommitmentMessage,
	error,
) {
	keyShare, err := ls.generateDsaKeyShare()
	if err != nil {
		return nil, fmt.Errorf(
			"could not generate DSA key shares [%v]", err,
		)
	}

	commitment, decommitmentKey, err := commitment.Generate(
		keyShare.publicKeyShare.Bytes(),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"could not generate DSA public key commitment [%v]", err,
		)
	}

	ls.dsaKeyShare = keyShare
	ls.publicDsaKeyShareDecommitmentKey = decommitmentKey

	return &PublicKeyShareCommitmentMessage{
		signerID:                 ls.ID,
		publicKeyShareCommitment: commitment,
	}, nil
}

// RevealDsaKeyShares produces a KeyShareRevealMessage and should be called
// when `PublicKeyShareCommitmentMessage`s from all group members are gathered.
//
// `KeyShareRevealMessage` contains signer's public DSA key share, decommitment
// key for this share (used to validate the commitment published in the previous
// `PublicKeyShareCommitmentMessage` message), encrypted secret DSA key share
// and ZKP for the secret key share correctness.
//
// Secret key share is encrypted with an additively homomorphic encryption
// scheme and sent to all other Signers in the group along with the public key
// share.
func (ls *LocalSigner) RevealDsaKeyShares() (*KeyShareRevealMessage, error) {
	paillierRandomness, err := paillier.GetRandomNumberInMultiplicativeGroup(
		ls.paillierKey.N, rand.Reader,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"could not generate random r for Paillier [%v]", err,
		)
	}

	encryptedSecretKeyShare, err := ls.paillierKey.EncryptWithR(
		ls.dsaKeyShare.secretKeyShare, paillierRandomness,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"could not encrypt secret key share [%v]", err,
		)
	}

	rangeProof, err := zkp.CommitDsaPaillierKeyRange(
		ls.dsaKeyShare.secretKeyShare,
		ls.dsaKeyShare.publicKeyShare,
		encryptedSecretKeyShare,
		paillierRandomness,
		ls.zkpParameters,
		rand.Reader,
	)

	return &KeyShareRevealMessage{
		signerID:                      ls.ID,
		secretKeyShare:                encryptedSecretKeyShare,
		publicKeyShare:                ls.dsaKeyShare.publicKeyShare,
		publicKeyShareDecommitmentKey: ls.publicDsaKeyShareDecommitmentKey,
		secretKeyProof:                rangeProof,
	}, nil
}

// CombineDsaKeyShares combines all group `PublicKeyShareCommitmentMessage`s and
// `KeyShareRevealMessage`s into a `ThresholdDsaKey` which is a (t, n) threshold
// sharing of an underlying secret DSA key. Secret and public
// DSA key shares are combined in the following way:
//
// E(secretKey) = E(secretKeyShare_1) + E(secretKeyShare_2) + ... + E(secretKeyShare_n)
// publicKey = publicKeyShare_1 + publicKeyShare_2 + ... + publicKeyShare_n
//
// E is an additively homomorphic encryption scheme, hence `+` operation is
// possible. Each key share share comes from the `KeyShareRevealMessage` that
// was sent by each `LocalSigner` of the signing group.
//
// Before shares are combined, messages are validated - we check whether
// the published public key share is what the signer originally committed to
// as well as we check validity of the secret key share using the provided ZKP.
//
// Every `PublicKeyShareCommitmentMessage` should have a corresponding
// `KeyShareRevealMessage`. They are matched by a signer ID contained in
// each of the messages.
func (ls *LocalSigner) CombineDsaKeyShares(
	shareCommitments []*PublicKeyShareCommitmentMessage,
	revealedShares []*KeyShareRevealMessage,
) (*ThresholdDsaKey, error) {
	if len(shareCommitments) != ls.groupParameters.GroupSize {
		return nil, fmt.Errorf(
			"commitments required from all group members; got %v, expected %v",
			len(shareCommitments),
			ls.groupParameters.GroupSize,
		)
	}

	if len(revealedShares) != ls.groupParameters.GroupSize {
		return nil, fmt.Errorf(
			"all group members should reveal shares; Got %v, expected %v",
			len(revealedShares),
			ls.groupParameters.GroupSize,
		)
	}

	secretKeyShares := make([]*paillier.Cypher, ls.groupParameters.GroupSize)
	publicKeyShares := make([]*curve.Point, ls.groupParameters.GroupSize)

	for i, commitmentMsg := range shareCommitments {
		foundMatchingRevealMessage := false

		for _, revealedSharesMsg := range revealedShares {

			if commitmentMsg.signerID == revealedSharesMsg.signerID {
				foundMatchingRevealMessage = true

				if revealedSharesMsg.isValid(
					commitmentMsg.publicKeyShareCommitment, ls.zkpParameters,
				) {
					secretKeyShares[i] = revealedSharesMsg.secretKeyShare
					publicKeyShares[i] = revealedSharesMsg.publicKeyShare
				} else {
					return nil, errors.New("KeyShareRevealMessage rejected")
				}
			}
		}

		if !foundMatchingRevealMessage {
			return nil, fmt.Errorf(
				"no matching share reveal message for signer with ID=%v",
				commitmentMsg.signerID,
			)
		}
	}

	secretKey := ls.paillierKey.Add(secretKeyShares...)
	publicKey := publicKeyShares[0]
	for _, share := range publicKeyShares[1:] {
		publicKey = curve.NewPoint(ls.groupParameters.Curve.Add(
			publicKey.X, publicKey.Y, share.X, share.Y,
		))
	}

	return &ThresholdDsaKey{secretKey, publicKey}, nil
}

func generateMemberID() string {
	memberID := "0"
	for memberID = fmt.Sprintf("%v", mathrand.Int31()); memberID == "0"; {
	}
	return memberID
}

// Round1Signer represents state of signer after executing the first round
// of signing algorithm.
type Round1Signer struct {
	Signer

	// Intermediate values stored between the first and second round of signing.
	secretKeyFactorShare                *big.Int                    // ρ_i
	encryptedSecretKeyFactorShare       *paillier.Cypher            // u_i = E(ρ_i)
	secretKeyMultipleShare              *paillier.Cypher            // v_i = E(ρ_i * x)
	secretKeyFactorShareDecommitmentKey *commitment.DecommitmentKey // D_1i
	paillierRandomness                  *big.Int
}

// SignRound1 executes the first round of T-ECDSA signing as described in
// [GGN 16], section 4.3.
//
// In the first round, each signer generates a secret key factor share `ρ_i`,
// encodes it with Paillier key `u_i = E(ρ_i)`, multiplies it with secret ECDSA
// key `v_i = E(ρ_i * x)` and publishes commitment for both those values
// `Com(u_i, v_i)`.
func (s *Signer) SignRound1() (*Round1Signer, *SignRound1Message, error) {
	// Choosing random ρ_i from Z_q
	secretKeyFactorShare, err := rand.Int(
		rand.Reader,
		s.groupParameters.curveCardinality(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"could not execute round 1 of signing [%v]", err,
		)
	}

	paillierRandomness, err := paillier.GetRandomNumberInMultiplicativeGroup(
		s.paillierKey.N, rand.Reader,
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"could not execute round 1 of signing [%v]", err,
		)
	}

	// u_i = E(ρ_i)
	encryptedSecretKeyFactorShare, err := s.paillierKey.EncryptWithR(
		secretKeyFactorShare, paillierRandomness,
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"could not execute round 1 of signing [%v]", err,
		)
	}

	// v_i = E(ρ_i * x)
	secretKeyMultiple := s.paillierKey.Mul(
		s.dsaKey.secretKey,
		secretKeyFactorShare,
	)

	// [C_1i, D_1i] = Com([u_i, v_i])
	commitment, decommitmentKey, err := commitment.Generate(
		encryptedSecretKeyFactorShare.C.Bytes(),
		secretKeyMultiple.C.Bytes(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"could not execute round 1 of signing [%v]", err,
		)
	}

	round1Signer := &Round1Signer{
		Signer:                              *s,
		secretKeyFactorShare:                secretKeyFactorShare,
		encryptedSecretKeyFactorShare:       encryptedSecretKeyFactorShare,
		secretKeyMultipleShare:              secretKeyMultiple,
		secretKeyFactorShareDecommitmentKey: decommitmentKey,
		paillierRandomness:                  paillierRandomness,
	}

	round1Message := &SignRound1Message{
		signerID:                       s.ID,
		secretKeyFactorShareCommitment: commitment,
	}

	return round1Signer, round1Message, nil
}

// Round2Signer represents state of signer after executing the second round
// of signing algorithm.
type Round2Signer struct {
	Signer
}

// SignRound2 executes the second round of T-ECDSA signing as described in
// [GGN 16], section 4.3.
//
// In the second round, encrypted secret key factor share `u_i = E(ρ_i)` and
// secret DSA key multiple `v_i = E(ρ_i * x)` are revealed along with
// a decommitment key `D_1i` allowing to check revealed values against the
// commitment published in the first round.
// Moreover, message produced in the second round contains a ZKP allowing to
// verify correctness of the revealed values.
func (s *Round1Signer) SignRound2() (*Round2Signer, *SignRound2Message, error) {
	zkp, err := zkp.CommitDsaPaillierSecretKeyFactorRange(
		s.secretKeyMultipleShare,
		s.dsaKey.secretKey,
		s.encryptedSecretKeyFactorShare,
		s.secretKeyFactorShare,
		s.paillierRandomness,
		s.zkpParameters,
		rand.Reader,
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"could not execute round 2 of signing [%v]", err,
		)
	}

	signer := &Round2Signer{s.Signer}

	round2Message := &SignRound2Message{
		signerID:                            s.ID,
		secretKeyFactorShare:                s.encryptedSecretKeyFactorShare,
		secretKeyMultipleShare:              s.secretKeyMultipleShare,
		secretKeyFactorShareDecommitmentKey: s.secretKeyFactorShareDecommitmentKey,
		secretKeyFactorProof:                zkp,
	}

	return signer, round2Message, nil
}

// CombineRound2Messages takes all messages from the first and second signing
// round, validates and combines them together in order to evaluate random
// secret key factor `u` and secret key multiple `v`:
//
// u = u_1 + u_2 + ... + u_n = E(ρ_1) + E(ρ_2) + ... + E(ρ_n)
// v = v_1 + v_2 + ... + v_n = E(ρ_1 * x) + E(ρ_2 * x) + ... + E(ρ_n * x)
//
// This function should be called before the `SignRound3` and the returned
// values should be used as a parameters to `SignRound3`.
func (s *Round2Signer) CombineRound2Messages(
	round1Messages []*SignRound1Message,
	round2Messages []*SignRound2Message,
) (
	secretKeyFactor *paillier.Cypher,
	secretKeyMultiple *paillier.Cypher,
	err error,
) {
	groupSize := s.groupParameters.GroupSize

	if len(round1Messages) != groupSize {
		return nil, nil, fmt.Errorf(
			"round 1 messages required from all group members; got %v, expected %v",
			len(round1Messages),
			groupSize,
		)
	}

	if len(round2Messages) != groupSize {
		return nil, nil, fmt.Errorf(
			"round 2 messages required from all group members; got %v, expected %v",
			len(round2Messages),
			groupSize,
		)
	}

	secretKeyFactorShares := make([]*paillier.Cypher, groupSize)
	secretKeyMultipleShares := make([]*paillier.Cypher, groupSize)

	for i, round1Message := range round1Messages {
		foundMatchingRound2Message := false

		for _, round2Message := range round2Messages {
			if round1Message.signerID == round2Message.signerID {
				foundMatchingRound2Message = true

				if round2Message.isValid(
					round1Message.secretKeyFactorShareCommitment,
					s.dsaKey.secretKey,
					s.zkpParameters,
				) {
					secretKeyFactorShares[i] = round2Message.secretKeyFactorShare
					secretKeyMultipleShares[i] = round2Message.secretKeyMultipleShare
				} else {
					return nil, nil, errors.New("round 2 message rejected")
				}
			}
		}

		if !foundMatchingRound2Message {
			return nil, nil, fmt.Errorf(
				"no matching round 2 message for signer with ID = %v",
				round1Message.signerID,
			)
		}
	}

	secretKeyFactor = s.paillierKey.Add(secretKeyFactorShares...)
	secretKeyMultiple = s.paillierKey.Add(secretKeyMultipleShares...)
	err = nil

	return
}

// Round3Signer represents state of signer after executing the third round
// of signing algorithm.
type Round3Signer struct {
	Signer

	secretKeyFactor   *paillier.Cypher // u = E(ρ)
	secretKeyMultiple *paillier.Cypher // v = E(ρx)

	// Intermediate values stored between the third and fourth round of signing
	signatureFactorSecretShare          *big.Int                    // k_i
	signatureFactorPublicShare          *curve.Point                // r_i = g^{k_i}
	signatureFactorMaskShare            *big.Int                    // c_i
	signatureUnmaskShare                *paillier.Cypher            // w_i = E(k_i * ρ + c_i * q)
	signatureFactorShareDecommitmentKey *commitment.DecommitmentKey // Com(r_i, w_i)
	paillierRandomness                  *big.Int
}

// SignRound3 executes the third round of T-ECDSA signing as described in
// [GGN 16], section 4.3.
//
// Before it executes all computations described in [GGN 16], it's required to
// combine messages from the previous two rounds in order to combine
// secret key factor shares and secret key multiple shares:
// u = u_1 + u_2 + ... + u_n = E(ρ_1) + E(ρ_2) + ... + E(ρ_n)
// v = v_1 + v_2 + ... + v_n = E(ρ_1 * x) + E(ρ_2 * x) + ... + E(ρ_n * x)
//
// To do that, please execute `CombineRound2Messages`` function and pass the
// returned values as an arguments to `SignRound3`.
func (s *Round2Signer) SignRound3(
	secretKeyFactor *paillier.Cypher, // u = E(ρ)
	secretKeyMultiple *paillier.Cypher, // v = E(ρx)
) (
	*Round3Signer, *SignRound3Message, error,
) {
	// k_i = rand(Z_q)
	signatureFactorSecretShare, err := rand.Int(
		rand.Reader,
		s.groupParameters.curveCardinality(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"could not execute round 3 of signing [%v]", err,
		)
	}

	// r_i = g^{k_i}
	signatureFactorPublicShare := curve.NewPoint(
		s.groupParameters.Curve.ScalarBaseMult(
			signatureFactorSecretShare.Bytes(),
		),
	)

	// c_i = rand[0, q^6)
	//
	// According to [GGN 16], `c_i` should be randomly chosen from
	// `[-q^6, q^6]`. Since `k_i` is chosen from [0, q), it means that in
	// a lot of cases, signature unmask will be a negative integer, since
	// `D(w) = k_i * rho + c_i * q`.
	// However, keep in mind, that Paillier encryption scheme does not allow for
	// encrypting negative integers by default since they are out of the allowed
	// plaintext space `[0, N)` where `N` is the Paillier modulus.
	// If we pick a negative integer as `c_i`, there is a high probability the
	// signature ZKP and final T-ECDSA signature will fail.
	// That's the reason why we decided to pick a random element from [0, q^6)
	// instead of from `[-q^6, q^6]`.
	signatureFactorMaskShare, err := rand.Int(
		rand.Reader,
		new(big.Int).Exp(
			s.groupParameters.curveCardinality(),
			big.NewInt(6),
			nil,
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"could not execute round 3 of signing [%v]", err,
		)
	}

	// w_i = E(k_i * ρ + c_i * q)
	paillierRandomness, err := paillier.GetRandomNumberInMultiplicativeGroup(
		s.paillierKey.N, rand.Reader,
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"could not execute round 3 of signing [%v]", err,
		)
	}
	maskShareMulCardinality, err := s.paillierKey.EncryptWithR(
		new(big.Int).Mul(
			signatureFactorMaskShare,
			s.groupParameters.curveCardinality(),
		),
		paillierRandomness,
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"could not execute round 3 of signing [%v]", err,
		)
	}
	signatureUnmaskShare := s.paillierKey.Add(
		s.paillierKey.Mul(secretKeyFactor, signatureFactorSecretShare),
		maskShareMulCardinality,
	)

	// [C_2i, D_2i] = Com(r_i, w_i)
	commitment, decommitmentKey, err :=
		commitment.Generate(
			signatureFactorPublicShare.Bytes(),
			signatureUnmaskShare.C.Bytes(),
		)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"could not execute round 3 of signing [%v]", err,
		)
	}

	signer := &Round3Signer{
		Signer: s.Signer,

		secretKeyFactor:   secretKeyFactor,
		secretKeyMultiple: secretKeyMultiple,

		signatureFactorSecretShare:          signatureFactorSecretShare,
		signatureFactorPublicShare:          signatureFactorPublicShare,
		signatureFactorMaskShare:            signatureFactorMaskShare,
		signatureUnmaskShare:                signatureUnmaskShare,
		signatureFactorShareDecommitmentKey: decommitmentKey,
		paillierRandomness:                  paillierRandomness,
	}

	round3Message := &SignRound3Message{
		signerID:                       s.ID,
		signatureFactorShareCommitment: commitment,
	}

	return signer, round3Message, nil
}

// Round4Signer represents state of signer after executing the fourth round
// of signing algorithm.
type Round4Signer struct {
	Signer

	secretKeyFactor   *paillier.Cypher // u = E(ρ)
	secretKeyMultiple *paillier.Cypher // v = E(ρx)
}

// SignRound4 executes the fourth round of T-ECDSA signing as described in
// [GGN 16], section 4.3.
//
// In the round 4, signer reveals signature factor public share
// (`r_i`), signature unmask share (`w_i`) evaluated in the previous round,
// decommitment key allowing to validate commitment to those values
// (published in the previous round) as well as ZKP allowing to check their
// correctness.
func (s *Round3Signer) SignRound4() (*Round4Signer, *SignRound4Message, error) {
	zkp, err := zkp.CommitEcdsaSignatureFactorRangeProof(
		s.signatureFactorPublicShare,
		s.signatureUnmaskShare,
		s.secretKeyFactor,
		s.signatureFactorSecretShare,
		s.signatureFactorMaskShare,
		s.paillierRandomness,
		s.zkpParameters,
		rand.Reader,
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"could not execute round 4 of signing [%v]", err,
		)
	}

	signer := &Round4Signer{
		Signer: s.Signer,

		secretKeyFactor:   s.secretKeyFactor,
		secretKeyMultiple: s.secretKeyMultiple,
	}

	round4Message := &SignRound4Message{
		signerID: s.ID,

		signatureFactorPublicShare:          s.signatureFactorPublicShare,
		signatureUnmaskShare:                s.signatureUnmaskShare,
		signatureFactorShareDecommitmentKey: s.signatureFactorShareDecommitmentKey,

		signatureFactorProof: zkp,
	}

	return signer, round4Message, nil
}

// CombineRound4Messages takes all messages from the third and fourth signing
// round, validates and combines them together in order to evaluate public
// signature factor `R` and signature unmask parameter `w`:
//
// w = w_1 + w_2 + ... + w_n = E(kρ + cq)
// R = r_1 + r_2 + ... + r_n = g^k
//
// This function should be called before the `SignRound5` and the returned
// values should be used as a parameters to `SignRound5`.
func (s *Round4Signer) CombineRound4Messages(
	round3Messages []*SignRound3Message,
	round4Messages []*SignRound4Message,
) (
	signatureUnmask *paillier.Cypher, // w
	signatureFactorPublic *curve.Point, // R
	err error,
) {
	groupSize := s.groupParameters.GroupSize

	if len(round3Messages) != groupSize {
		return nil, nil, fmt.Errorf(
			"round 3 messages required from all group members; got %v, expected %v",
			len(round3Messages),
			groupSize,
		)
	}

	if len(round4Messages) != groupSize {
		return nil, nil, fmt.Errorf(
			"round 4 messages required from all group members; got %v, expected %v",
			len(round4Messages),
			groupSize,
		)
	}

	signatureUnmaskShares := make([]*paillier.Cypher, groupSize)
	signatureFactorPublicShares := make([]*curve.Point, groupSize)

	for i, round3Message := range round3Messages {
		foundMatchingRound4Message := false

		for _, round4Message := range round4Messages {
			if round3Message.signerID == round4Message.signerID {
				foundMatchingRound4Message = true

				if round4Message.isValid(
					round3Message.signatureFactorShareCommitment,
					s.secretKeyFactor,
					s.zkpParameters,
				) {
					signatureFactorPublicShares[i] = round4Message.signatureFactorPublicShare
					signatureUnmaskShares[i] = round4Message.signatureUnmaskShare
				} else {
					return nil, nil, errors.New("round 4 message rejected")
				}
			}
		}

		if !foundMatchingRound4Message {
			return nil, nil, fmt.Errorf(
				"no matching round 4 message for signer with ID = %v",
				round3Message.signerID,
			)
		}
	}

	// w = w_1 + w_2 + ... + w_n
	signatureUnmask = s.paillierKey.Add(signatureUnmaskShares...)

	// R = r_i + r_2 + ... + r_n
	signatureFactorPublic = signatureFactorPublicShares[0]
	for _, share := range signatureFactorPublicShares[1:] {
		signatureFactorPublic = curve.NewPoint(
			s.groupParameters.Curve.Add(
				signatureFactorPublic.X,
				signatureFactorPublic.Y,
				share.X,
				share.Y,
			))
	}

	err = nil

	return
}

// Round5Signer represents state of `Signer` after executing the fifth round
// of signing algorithm.
type Round5Signer struct {
	Signer

	secretKeyFactor           *paillier.Cypher // u = E(ρ)
	secretKeyMultiple         *paillier.Cypher // v = E(ρx)
	signatureUnmask           *paillier.Cypher // w
	signatureFactorPublic     *curve.Point     // R
	signatureFactorPublicHash *big.Int         // r = H'(R)
}

// SignRound5 executes the fifth round of signing. In the fifth round, signers
// jointly decrypt signature unmask `w` as well as compute hash of the signature
// factor public parameter. Both values will be used in round six when
// evaluating the final signature.
func (s *Round4Signer) SignRound5(
	signatureUnmask *paillier.Cypher, // w
	signatureFactorPublic *curve.Point, // R
) (
	*Round5Signer, *SignRound5Message, error,
) {

	// TDec(w) share
	signatureUnmaskPartialDecryption := s.paillierKey.Decrypt(signatureUnmask.C)

	// r = H'(R)
	//
	// According to [GGN 16], H' is a hash function defined from `G` to `Z_q`.
	// It does not have to be a cryptographic hash function, so we use the
	// simplest possible form here.
	signatureFactorPublicHash := new(big.Int).Mod(
		signatureFactorPublic.X,
		s.groupParameters.curveCardinality(),
	)

	message := &SignRound5Message{
		signerID: s.ID,

		signatureUnmaskPartialDecryption: signatureUnmaskPartialDecryption,
	}

	signer := &Round5Signer{
		Signer: s.Signer,

		secretKeyFactor:           s.secretKeyFactor,
		secretKeyMultiple:         s.secretKeyMultiple,
		signatureUnmask:           signatureUnmask,
		signatureFactorPublic:     signatureFactorPublic,
		signatureFactorPublicHash: signatureFactorPublicHash,
	}

	return signer, message, nil
}

// CombineRound5Messages combines together all `SignRound5Message`s produced by
// signers. Each message contains a partial decryption for signature unmask
// parameter `w`. Function combines them together and returns a final decrypted
// value of signature unmask.
func (s *Round5Signer) CombineRound5Messages(
	round5Messages []*SignRound5Message,
) (
	signatureUnmask *big.Int, // TDec(w)
	err error,
) {
	groupSize := s.groupParameters.GroupSize

	if len(round5Messages) != groupSize {
		return nil, fmt.Errorf(
			"round 5 messages required from all group members; got %v, expected %v",
			len(round5Messages),
			groupSize,
		)
	}

	partialDecryptions := make([]*paillier.PartialDecryption, groupSize)
	for i, round5Message := range round5Messages {
		partialDecryptions[i] = round5Message.signatureUnmaskPartialDecryption
	}

	signatureUnmask, err = s.paillierKey.CombinePartialDecryptions(
		partialDecryptions,
	)
	if err != nil {
		err = fmt.Errorf(
			"could not combine signature unmask partial decryptions [%v]",
			err,
		)
	}

	return
}

// SignRound6 executes the sixth round of signing. In the sixth round, all
// parameters signers evaluates so far are combined together in order to produce
// a final signature. The final signature is in a Paillier-encrypted form, so
// a threshold decode action must be performed.
func (s *Round5Signer) SignRound6(
	signatureUnmask *big.Int, // TDec(w)
	messageHash []byte, // m
) (*SignRound6Message, error) {
	if len(messageHash) != 32 {
		return nil, fmt.Errorf(
			"message hash is required to be exactly 32 bytes and it's %d bytes",
			len(messageHash),
		)
	}

	signatureCypher := s.paillierKey.Mul(
		s.paillierKey.Add(
			s.paillierKey.Mul(
				s.secretKeyFactor,
				new(big.Int).SetBytes(messageHash[:]),
			),
			s.paillierKey.Mul(
				s.secretKeyMultiple,
				s.signatureFactorPublicHash,
			),
		),
		new(big.Int).ModInverse(
			signatureUnmask,
			s.groupParameters.curveCardinality(),
		),
	)

	return &SignRound6Message{
		signaturePartialDecryption: s.paillierKey.Decrypt(signatureCypher.C),
	}, nil
}

// Signature represents a final T-ECDSA signature
type Signature struct {
	R *big.Int
	S *big.Int
}

// CombineRound6Messages combines together all partial decryptions of signature
// generated in the sixth round of signing. It outputs a final T-ECDSA signature
// in an unencrypted form.
func (s *Round5Signer) CombineRound6Messages(
	round6Messages []*SignRound6Message,
) (*Signature, error) {
	groupSize := s.groupParameters.GroupSize

	if len(round6Messages) != groupSize {
		return nil, fmt.Errorf(
			"round 6 messages required from all group members; got %v, expected %v",
			len(round6Messages),
			groupSize,
		)
	}

	partialDecryptions := make([]*paillier.PartialDecryption, groupSize)
	for i, round6Message := range round6Messages {
		partialDecryptions[i] = round6Message.signaturePartialDecryption
	}

	sign, err := s.paillierKey.CombinePartialDecryptions(
		partialDecryptions,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"could not combine signature partial decryptions [%v]",
			err,
		)
	}

	sign = new(big.Int).Mod(sign, s.groupParameters.curveCardinality())

	// Inherent ECDSA signature malleability
	// BTC and ETH require that the S value inside ECDSA signatures is at most
	// the curve order divided by 2 (essentially restricting this value to its
	// lower half range).
	halfOrder := new(big.Int).Rsh(s.groupParameters.curveCardinality(), 1)
	if sign.Cmp(halfOrder) == 1 {
		sign = new(big.Int).Sub(s.groupParameters.curveCardinality(), sign)
	}

	return &Signature{
		R: s.signatureFactorPublicHash,
		S: sign,
	}, nil
}
