//
// Copyright 2021 The Sigstore Authors.
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

package verify

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	_ "crypto/sha256" // for `crypto.SHA256`
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"

	"github.com/pkg/errors"
	"github.com/sigstore/cosign/cmd/cosign/cli/fulcio"
	"github.com/sigstore/cosign/cmd/cosign/cli/options"
	"github.com/sigstore/cosign/cmd/cosign/cli/sign"
	"github.com/sigstore/cosign/pkg/blob"
	"github.com/sigstore/cosign/pkg/cosign"
	"github.com/sigstore/cosign/pkg/cosign/pivkey"
	"github.com/sigstore/cosign/pkg/cosign/pkcs11key"
	sigs "github.com/sigstore/cosign/pkg/signature"
	rekorClient "github.com/sigstore/rekor/pkg/client"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	sigstoresigs "github.com/sigstore/sigstore/pkg/signature"
	signatureoptions "github.com/sigstore/sigstore/pkg/signature/options"
)

// nolint
func VerifyStringCmd(ctx context.Context, ko sign.KeyOpts, certRef, sigRef, bytesRef string) error {
	var pubKey sigstoresigs.Verifier
	var err error
	var cert *x509.Certificate

	if !options.OneOf(ko.KeyRef, ko.Sk, certRef) && !options.EnableExperimental() {
		return &options.PubKeyParseError{}
	}

	var b64sig string
	if sigRef == "" {
		return fmt.Errorf("missing flag '--signature'")
	}
	targetSig, err := blob.LoadFileOrURL(sigRef)
	if err != nil {
		if !os.IsNotExist(err) {
			// ignore if file does not exist, it can be a base64 encoded string as well
			return err
		}
		targetSig = []byte(sigRef)
	}

	if isb64(targetSig) {
		b64sig = string(targetSig)
	} else {
		b64sig = base64.StdEncoding.EncodeToString(targetSig)
	}

	blobBytes := []byte(bytesRef)

	// Keys are optional!
	switch {
	case ko.KeyRef != "":
		pubKey, err = sigs.PublicKeyFromKeyRef(ctx, ko.KeyRef)
		if err != nil {
			return errors.Wrap(err, "loading public key")
		}
		pkcs11Key, ok := pubKey.(*pkcs11key.Key)
		if ok {
			defer pkcs11Key.Close()
		}
	case ko.Sk:
		sk, err := pivkey.GetKeyWithSlot(ko.Slot)
		if err != nil {
			return errors.Wrap(err, "opening piv token")
		}
		defer sk.Close()
		pubKey, err = sk.Verifier()
		if err != nil {
			return errors.Wrap(err, "loading public key from token")
		}
	case certRef != "":
		pems, err := os.ReadFile(certRef)
		if err != nil {
			return err
		}

		var out []byte
		out, err = base64.StdEncoding.DecodeString(string(pems))
		if err != nil {
			// not a base64
			out = pems
		}

		certs, err := cryptoutils.UnmarshalCertificatesFromPEM(out)
		if err != nil {
			return err
		}
		if len(certs) == 0 {
			return errors.New("no certs found in pem file")
		}
		cert = certs[0]
		pubKey, err = sigstoresigs.LoadECDSAVerifier(cert.PublicKey.(*ecdsa.PublicKey), crypto.SHA256)
		if err != nil {
			return err
		}
	case options.EnableExperimental():
		rClient, err := rekorClient.GetRekorClient(ko.RekorURL)
		if err != nil {
			return err
		}

		uuids, err := cosign.FindTLogEntriesByPayload(ctx, rClient, blobBytes)
		if err != nil {
			return err
		}

		if len(uuids) == 0 {
			return errors.New("could not find a tlog entry for provided blob")
		}

		tlogEntry, err := cosign.GetTlogEntry(ctx, rClient, uuids[0])
		if err != nil {
			return err
		}

		certs, err := extractCerts(tlogEntry)
		if err != nil {
			return err
		}
		cert = certs[0]
		pubKey, err = sigstoresigs.LoadECDSAVerifier(cert.PublicKey.(*ecdsa.PublicKey), crypto.SHA256)
		if err != nil {
			return err
		}
	}

	sig, err := base64.StdEncoding.DecodeString(b64sig)
	if err != nil {
		return err
	}
	if err := pubKey.VerifySignature(bytes.NewReader(sig), bytes.NewReader(blobBytes)); err != nil {
		return err
	}

	if cert != nil { // cert
		if err := cosign.TrustedCert(cert, fulcio.GetRoots()); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Certificate is trusted by Fulcio Root CA")
		fmt.Fprintln(os.Stderr, "Email:", cert.EmailAddresses)
		for _, uri := range cert.URIs {
			fmt.Fprintf(os.Stderr, "URI: %s://%s%s\n", uri.Scheme, uri.Host, uri.Path)
		}
		fmt.Fprintln(os.Stderr, "Issuer: ", sigs.CertIssuerExtension(cert))
	}
	fmt.Fprintln(os.Stderr, "Verified OK")

	if options.EnableExperimental() {
		rekorClient, err := rekorClient.GetRekorClient(ko.RekorURL)
		if err != nil {
			return err
		}
		var pubBytes []byte
		if pubKey != nil {
			pubBytes, err = sigs.PublicKeyPem(pubKey, signatureoptions.WithContext(ctx))
			if err != nil {
				return err
			}
		}
		if cert != nil {
			pubBytes, err = cryptoutils.MarshalCertificateToPEM(cert)
			if err != nil {
				return err
			}
		}
		uuid, index, err := cosign.FindTlogEntry(ctx, rekorClient, b64sig, blobBytes, pubBytes)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "tlog entry verified with uuid: %q index: %d\n", uuid, index)
		return nil
	}

	return nil
}