// Copyright 2016, 2017 Thales e-Security, Inc
//
// Permission is hereby granted, free of charge, to any person obtaining
// a copy of this software and associated documentation files (the
// "Software"), to deal in the Software without restriction, including
// without limitation the rights to use, copy, modify, merge, publish,
// distribute, sublicense, and/or sell copies of the Software, and to
// permit persons to whom the Software is furnished to do so, subject to
// the following conditions:
//
// The above copyright notice and this permission notice shall be
// included in all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
// NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE
// LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION
// OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION
// WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

// Access cryptographic keys from PKCS#11 using Go crypto API.
//
// For simple use:
//
// 1. Either write a configuration file (see ConfigureFromFile) or
// define a configuration in your application (see PKCS11Config and
// Configure). This will identify the PKCS#11 library and token to
// use, and contain the password (or "PIN" in PKCS#11 terminology) to
// use if the token requires login.
//
// 2. Create keys with GenerateDSAKeyPair, GenerateRSAKeyPair and
// GenerateECDSAKeyPair. The keys you get back implement the standard
// Go crypto.Signer interface (and crypto.Decrypter, for RSA). They
// are automatically persisted under random a randomly generated label
// and ID (use the Identify method to discover them).
//
// 3. Retrieve existing keys with FindKeyPair. The return value is a
// Go crypto.PrivateKey; it may be converted either to crypto.Signer
// or to *PKCS11PrivateKeyDSA, *PKCS11PrivateKeyECDSA or
// *PKCS11PrivateKeyRSA.
//
// Sessions and concurrency:
//
// Note that PKCS#11 session handles must not be used concurrently
// from multiple threads. Consumers of the Signer interface know
// nothing of this and expect to be able to sign from multiple threads
// without constraint. We address this as follows.
//
// 1. PKCS11Object captures both the object handle and the slot ID
// for an object.
//
// 2. For each slot we maintain a pool of read-write sessions. The
// pool expands dynamically up to an (undocumented) limit.
//
// 3. Each operation transiently takes a session from the pool. They
// have exclusive use of the session, meeting PKCS#11's concurrency
// requirements.
//
// The details are, partially, exposed in the API; since the target
// use case is PKCS#11-unaware operation it may be that the API as it
// stands isn't good enough for PKCS#11-aware applications. Feedback
// welcome.
//
// See also https://golang.org/pkg/crypto/
package crypto11

import (
	"crypto"
	"encoding/json"
	"errors"
	"github.com/miekg/pkcs11"
	"log"
	"os"
)

// ErrTokenNotFound represents the failure to find the requested PKCS#11 token
var ErrTokenNotFound = errors.New("crypto11: could not find PKCS#11 token")

// ErrKeyNotFound represents the failure to find the requested PKCS#11 key
var ErrKeyNotFound = errors.New("crypto11: could not find PKCS#11 key")

// ErrNotConfigured is returned when the PKCS#11 library is not configured
var ErrNotConfigured = errors.New("crypto11: PKCS#11 not yet configured")

// ErrCannotOpenPKCS11 is returned when the PKCS#11 library cannot be opened
var ErrCannotOpenPKCS11 = errors.New("crypto11: could not open PKCS#11")

// ErrCannotGetRandomData is returned when the PKCS#11 library fails to return enough random data
var ErrCannotGetRandomData = errors.New("crypto11: cannot get random data from PKCS#11")

// ErrUnsupportedKeyType is returned when the PKCS#11 library returns a key type that isn't supported
var ErrUnsupportedKeyType = errors.New("crypto11: unrecognized key type")

// PKCS11Object contains a reference to a loaded PKCS#11 object.
type PKCS11Object struct {
	// The PKCS#11 object handle.
	Handle pkcs11.ObjectHandle

	// The PKCS#11 slot number.
	//
	// This is used internally to find a session handle that can
	// access this object.
	Slot uint
}

// PKCS11PrivateKey contains a reference to a loaded PKCS#11 private key object.
type PKCS11PrivateKey struct {
	PKCS11Object

	// The corresponding public key
	PubKey crypto.PublicKey
}

// In a former design we carried around the object handle for the
// public key and retrieved it on demand.  The downside of that is
// that the Public() method on Signer &c has no way to communicate
// errors.

/* Nasty globals */
var libHandle *pkcs11.Ctx
var session pkcs11.SessionHandle
var defaultSlot uint

// Find a token given its serial number
func findToken(slots []uint, serial string, label string) (uint, uint, error) {
	for _, slot := range slots {
		tokenInfo, err := libHandle.GetTokenInfo(slot)
		if err != nil {
			return 0, 0, err
		}
		if tokenInfo.SerialNumber == serial {
			return slot, tokenInfo.Flags, nil
		}
		if tokenInfo.Label == label {
			return slot, tokenInfo.Flags, nil
		}
	}
	return 0, 0, ErrTokenNotFound
}

// PKCS11Config holds PKCS#11 configuration information.
//
// A token may be identified either by serial number or label.  If
// both are specified then the first match wins.
//
// Supply this to Configure(), or alternatively use ConfigureFromFile().
type PKCS11Config struct {
	// Full path to PKCS#11 library
	Path string

	// Token serial number
	TokenSerial string

	// Token label
	TokenLabel string

	// User PIN (password)
	Pin string

	// Max token session
	MaxTokenSession int
}

// Configure configures PKCS#11 from a PKCS11Config.
//
// The PKCS#11 library context is returned,
// allowing a PKCS#11-aware application to make use of it. Non-aware
// appliations may ignore it.
//
// Unsually, these values may be present even if the error is
// non-nil. This corresponds to the case that the library has already
// been configured. Note that it is NOT reconfigured so if you supply
// a different configuration the second time, it will be ignored in
// favor of the first configuration.
//
// If config is nil, and the library has already been configured, the
// context from the first configuration is returned (and
// the error will be nil in this case).
func Configure(config *PKCS11Config) (*pkcs11.Ctx, error) {
	var err error
	var slots []uint
	var flags uint

	if config == nil {
		if libHandle != nil {
			return libHandle, nil
		}
		return nil, ErrNotConfigured
	}
	if libHandle != nil {
		return libHandle, nil
	}
	libHandle = pkcs11.New(config.Path)
	if libHandle == nil {
		log.Printf("Could not open PKCS#11 library: %s", config.Path)
		return nil, ErrCannotOpenPKCS11
	}
	if err = libHandle.Initialize(); err != nil {
		log.Printf("Failed to initialize PKCS#11 library: %s", err.Error())
		return nil, err
	}
	if slots, err = libHandle.GetSlotList(true); err != nil {
		log.Printf("Failed to list PKCS#11 Slots: %s", err.Error())
		return nil, err
	}
	if defaultSlot, flags, err = findToken(slots, config.TokenSerial, config.TokenLabel); err != nil {
		log.Printf("Failed to find Token in any Slot: %s", err.Error())
		return nil, err
	}
	if err = setupSessions(defaultSlot, config.MaxTokenSession); err != nil {
		return nil, err
	}
	if err = withSession(defaultSlot, func(session pkcs11.SessionHandle) error {
		if flags&pkcs11.CKF_LOGIN_REQUIRED != 0 {
			err = libHandle.Login(session, pkcs11.CKU_USER, config.Pin)
			if err != nil {
				log.Printf("Failed to login into PKCS#11 Token: %s", err.Error())
			}
		} else {
			err = nil
		}
		return err
	}); err != nil {
		log.Printf("Failed to open PKCS#11 Session: %s", err.Error())
		return nil, err
	}
	return libHandle, nil
}

// ConfigureFromFile configures PKCS#11 from a name configuration file.
//
// Configuration files are a JSON representation of the PKCSConfig object.
// The return value is as for Configure().
//
// Note that if CRYPTO11_CONFIG_PATH is set in the environment,
// configuration will be read from that file, overriding any later
// runtime configuration.
func ConfigureFromFile(configLocation string) (*pkcs11.Ctx, error) {
	file, err := os.Open(configLocation)
	if err != nil {
		log.Printf("Could not open config file: %s", configLocation)
		return nil, err
	}
	defer file.Close()
	configDecoder := json.NewDecoder(file)
	config := &PKCS11Config{}
	err = configDecoder.Decode(config)
	if err != nil {
		log.Printf("Could decode config file: %s", err.Error())
		return nil, err
	}
	return Configure(config)
}

func init() {
	if configLocation, ok := os.LookupEnv("CRYPTO11_CONFIG_PATH"); ok {
		if _, err := ConfigureFromFile(configLocation); err != nil {
			panic(err)
		}
	}
}
