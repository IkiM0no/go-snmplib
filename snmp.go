// Package snmplib provides an SNMP query and trapper library.
package snmplib

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/md5"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"reflect"
	"strings"
	"time"
)

// V3user object.
type V3user struct {
	User    string
	AuthAlg string //MD5 or SHA1
	AuthPwd string
	PrivAlg string //AES or DES
	PrivPwd string
}

// SNMP object type that lets you do SNMP requests.
type SNMP struct {
	Target    string        // Target device for these SNMP events.
	Community string        // Community to use to contact the device.
	Version   SNMPVersion   // SNMPVersion to encode in the packets.
	timeout   time.Duration // Timeout to use for all SNMP packets.
	retries   int           // Number of times to retry an operation.
	conn      net.Conn      // Cache the UDP connection in the object.

	//SNMP V3 variables
	user     string
	authAlg  string //MD5 or SHA1
	authPwd  string
	privAlg  string //AES or DES
	privPwd  string
	engineID string

	//V3 temp variables
	authKey     string
	privKey     string
	engineBoots int32
	engineTime  int32
	desIV       uint32
	aesIV       int64
	TrapUsers   []V3user
}

// SNMP constants.
const (
	bufSize    int    = 16384
	maxMsgSize int    = 65500
	SnmpAES    string = "AES"
	SnmpDES    string = "DES"
	SnmpSHA1   string = "SHA1"
	SnmpMD5    string = "MD5"
)

func passwordToKey(password string, engineID string, hashAlg string) string {
	h := sha1.New()
	if hashAlg == "MD5" {
		h = md5.New()
	}

	count := 0
	plen := len(password)
	repeat := 1048576 / plen
	remain := 1048576 % plen
	for count < repeat {
		io.WriteString(h, password)
		count++
	}
	if remain > 0 {
		io.WriteString(h, string(password[:remain]))
	}
	ku := string(h.Sum(nil))
	//fmt.Printf("ku=% x\n", ku)

	h.Reset()
	io.WriteString(h, ku)
	io.WriteString(h, engineID)
	io.WriteString(h, ku)
	localKey := h.Sum(nil)
	//fmt.Printf("localKey=% x\n", localKey)

	return string(localKey)
}

// NewSNMP creates a new SNMP object. Opens a UDP connection to the device that will be used for the SNMP packets.
func NewSNMP(target, community string, version SNMPVersion, timeout time.Duration, retries int) (*SNMP, error) {
	targetPort := fmt.Sprintf("%s:161", target)
	conn, err := net.DialTimeout("udp", targetPort, timeout)
	if err != nil {
		return nil, fmt.Errorf(`error connecting to ("udp", "%s") : %s`, targetPort, err)
	}
	return &SNMP{
		Target:    target,
		Community: community,
		Version:   version,
		timeout:   timeout,
		retries:   retries,
		conn:      conn,
	}, nil
}

// NewSNMPv3 creates a new SNMP object for SNMPv3. Opens a UDP connection to the device that will be used for the SNMP packets.
func NewSNMPv3(target, user, authAlg, authPwd, privAlg, privPwd string, timeout time.Duration, retries int) (*SNMP, error) {
	if authAlg != SnmpMD5 && authAlg != SnmpSHA1 {
		return nil, fmt.Errorf(`Invalid auth algorithm %s, needs SHA1 or MD5`, authAlg)
	}
	if privAlg != SnmpAES && privAlg != SnmpDES {
		return nil, fmt.Errorf(`Invalid priv algorithm %s, needs AES or DES`, privAlg)
	}

	targetPort := fmt.Sprintf("%s:161", target)
	conn, err := net.DialTimeout("udp", targetPort, timeout)
	if err != nil {
		return nil, fmt.Errorf(`error connecting to ("udp", "%s") : %s`, targetPort, err)
	}
	return &SNMP{
		Target:  target,
		Version: SNMPv3,
		timeout: timeout,
		retries: retries,
		conn:    conn,
		user:    user,
		authAlg: authAlg,
		authPwd: authPwd,
		privAlg: privAlg,
		privPwd: privPwd,
	}, nil
}

// NewSNMPOnConn creates a new SNMP object from an existing net.Conn. It does not check if the provided target is valid.
func NewSNMPOnConn(target, community string, version SNMPVersion, timeout time.Duration, retries int, conn net.Conn) *SNMP {
	return &SNMP{
		Target:    target,
		Community: community,
		Version:   version,
		timeout:   timeout,
		retries:   retries,
		conn:      conn,
	}
}

// Generate a valid SNMP request ID.
func getRandomRequestID() int {
	return int(rand.Int31())
}

// poll sends a packet and wait for a response. Both operations can timeout, they're retried up to retries times.
func poll(conn net.Conn, toSend []byte, respondBuffer []byte, retries int, timeout time.Duration) (int, error) {
	var err error
	for i := 0; i < retries+1; i++ {
		deadline := time.Now().Add(timeout)

		if err = conn.SetWriteDeadline(deadline); err != nil {
			log.Printf("Couldn't set write deadline. Retrying. Retry %d/%d\n", i, retries)
			continue
		}
		if _, err = conn.Write(toSend); err != nil {
			log.Printf("Couldn't write. Retrying. Retry %d/%d\n", i, retries)
			continue
		}

		deadline = time.Now().Add(timeout)
		if err = conn.SetReadDeadline(deadline); err != nil {
			log.Printf("Couldn't set read deadline. Retrying. Retry %d/%d\n", i, retries)
			continue
		}

		numRead := 0
		if numRead, err = conn.Read(respondBuffer); err != nil {
			log.Printf("Couldn't read. Retrying. Retry %d/%d\n", i, retries)
			continue
		}

		return numRead, nil
	}
	return 0, err
}

// Get sends an SNMP get request requesting the value for an oid.
func (w SNMP) Get(oid Oid) (interface{}, error) {
	requestID := getRandomRequestID()
	req, err := EncodeSequence([]interface{}{Sequence, int(w.Version), w.Community,
		[]interface{}{AsnGetRequest, requestID, 0, 0,
			[]interface{}{Sequence,
				[]interface{}{Sequence, oid, nil}}}})
	if err != nil {
		return nil, err
	}

	response := make([]byte, bufSize, bufSize)
	numRead, err := poll(w.conn, req, response, w.retries, 500*time.Millisecond)
	if err != nil {
		return nil, err
	}

	decodedResponse, err := DecodeSequence(response[:numRead])
	if err != nil {
		return nil, err
	}

	// Fetch the varbinds out of the packet.
	respPacket := decodedResponse[3].([]interface{})
	varbinds := respPacket[4].([]interface{})
	result := varbinds[1].([]interface{})[2]

	return result, nil
}

// GetMultiple issues a single GET SNMP request requesting multiple values
func (w SNMP) GetMultiple(oids []Oid) (map[string]interface{}, error) {
	requestID := getRandomRequestID()

	varbinds := []interface{}{Sequence}
	for _, oid := range oids {
		varbinds = append(varbinds, []interface{}{Sequence, oid, nil})
	}
	req, err := EncodeSequence([]interface{}{Sequence, int(w.Version), w.Community,
		[]interface{}{AsnGetRequest, requestID, 0, 0, varbinds}})

	if err != nil {
		return nil, err
	}

	response := make([]byte, bufSize, bufSize)
	numRead, err := poll(w.conn, req, response, w.retries, 500*time.Millisecond)
	if err != nil {
		return nil, err
	}

	decodedResponse, err := DecodeSequence(response[:numRead])
	if err != nil {
		return nil, err
	}

	// Find the varbinds
	respPacket := decodedResponse[3].([]interface{})
	respVarbinds := respPacket[4].([]interface{})

	result := make(map[string]interface{})
	for _, v := range respVarbinds[1:] { // First element is just a sequence
		oid := v.([]interface{})[1].(Oid).String()
		value := v.([]interface{})[2]
		result[oid] = value
	}

	return result, nil
}

// Discover : SNMP V3 requires a discover packet being sent before a request being sent,
// so that agent's engineID and other parameters can be automatically detected.
func (w *SNMP) Discover() error {
	msgID := getRandomRequestID()
	requestID := getRandomRequestID()
	v3Header, _ := EncodeSequence([]interface{}{Sequence, "", 0, 0, "", "", ""})
	flags := string([]byte{4})
	USM := 0x03
	req, err := EncodeSequence([]interface{}{
		Sequence, int(w.Version),
		[]interface{}{Sequence, msgID, maxMsgSize, flags, USM},
		string(v3Header),
		[]interface{}{Sequence, "", "",
			[]interface{}{AsnGetRequest, requestID, 0, 0, []interface{}{Sequence}}}})
	if err != nil {
		fmt.Printf("Error encoding in discover:%v\n", err)
		panic(err)
	}

	response := make([]byte, bufSize)
	numRead, err := poll(w.conn, req, response, w.retries, 500*time.Millisecond)
	if err != nil {
		return err
	}

	decodedResponse, err := DecodeSequence(response[:numRead])
	if err != nil {
		fmt.Printf("Error decoding discover:%v\n", err)
		panic(err)
	}

	v3HeaderStr := decodedResponse[3].(string)
	v3HeaderDecoded, err := DecodeSequence([]byte(v3HeaderStr))
	if err != nil {
		fmt.Printf("Error 2 decoding:%v\n", err)
		return err
	}

	w.engineID = v3HeaderDecoded[1].(string)
	w.engineBoots = int32(v3HeaderDecoded[2].(int))
	w.engineTime = int32(v3HeaderDecoded[3].(int))
	w.aesIV = rand.Int63()
	w.desIV = rand.Uint32()
	//keys
	w.authKey = passwordToKey(w.authPwd, w.engineID, w.authAlg)
	privKey := passwordToKey(w.privPwd, w.engineID, w.authAlg)
	w.privKey = string(([]byte(privKey))[0:16])
	return nil
}

func encryptDESCBC(dst, src, key, iv []byte) error {
	desBlockEncrypter, err := des.NewCipher([]byte(key))
	if err != nil {
		return err
	}
	desEncrypter := cipher.NewCBCEncrypter(desBlockEncrypter, iv)
	desEncrypter.CryptBlocks(dst, src)
	return nil
}

func decryptDESCBC(dst, src, key, iv []byte) error {
	desBlockEncrypter, err := des.NewCipher([]byte(key))
	if err != nil {
		return err
	}
	desDecrypter := cipher.NewCBCDecrypter(desBlockEncrypter, iv)
	desDecrypter.CryptBlocks(dst, src)
	return nil
}

func encryptAESCFB(dst, src, key, iv []byte) error {
	aesBlockEncrypter, err := aes.NewCipher([]byte(key))
	if err != nil {
		return err
	}
	aesEncrypter := cipher.NewCFBEncrypter(aesBlockEncrypter, iv)
	aesEncrypter.XORKeyStream(dst, src)
	return nil
}

func decryptAESCFB(dst, src, key, iv []byte) error {
	aesBlockDecrypter, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil
	}
	aesDecrypter := cipher.NewCFBDecrypter(aesBlockDecrypter, iv)
	aesDecrypter.XORKeyStream(dst, src)
	return nil
}

func strXor(s1, s2 string) string {
	if len(s1) != len(s2) {
		panic("strXor called with two strings of different length\n")
	}
	n := len(s1)
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = s1[i] ^ s2[i]
	}
	return string(b)
}

func (w SNMP) auth(wholeMsg string) string {
	//Auth
	padLen := 64 - len(w.authKey)
	eAuthKey := w.authKey + strings.Repeat("\x00", padLen)
	ipad := strings.Repeat("\x36", 64)
	opad := strings.Repeat("\x5C", 64)
	k1 := strXor(eAuthKey, ipad)
	k2 := strXor(eAuthKey, opad)
	h := sha1.New()
	if w.authAlg == "MD5" {
		h = md5.New()
	}
	io.WriteString(h, k1+wholeMsg)
	tmp1 := string(h.Sum(nil))
	h.Reset()
	io.WriteString(h, k2+tmp1)
	msgAuthParam := string(h.Sum(nil)[:12])
	return msgAuthParam
}

func (w SNMP) encrypt(payload string) (string, string, error) {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, w.engineBoots)
	if w.privAlg == SnmpAES {
		buf2 := new(bytes.Buffer)
		binary.Write(buf2, binary.BigEndian, w.engineTime)
		buf3 := new(bytes.Buffer)
		w.aesIV++
		binary.Write(buf3, binary.BigEndian, w.aesIV)
		privParam := string(buf3.Bytes())
		iv := string(buf.Bytes()) + string(buf2.Bytes()) + privParam

		// AES Encrypt
		encrypted := make([]byte, len(payload))
		err := encryptAESCFB(encrypted, []byte(payload), []byte(w.privKey), []byte(iv))
		if err != nil {
			return "", "", err
		}
		return string(encrypted), privParam, nil
	}

	desKey := w.privKey[:8]
	preIV := w.privKey[8:16]
	buf2 := new(bytes.Buffer)
	w.desIV++
	binary.Write(buf2, binary.BigEndian, w.desIV)
	privParam := string(buf.Bytes()) + string(buf2.Bytes())
	iv := strXor(preIV, privParam)

	//DES Encrypt
	plen := len(payload)
	//padding
	if (plen % 8) != 0 {
		payload = payload + strings.Repeat("\x00", 8-(plen%8))
	}
	encrypted := make([]byte, len(payload))
	encryptDESCBC(encrypted, []byte(payload), []byte(desKey), []byte(iv))
	return string(encrypted), privParam, nil
}

func (w SNMP) decrypt(payload, privParam string) (string, error) {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, w.engineBoots)

	if w.privAlg == SnmpAES {
		buf2 := new(bytes.Buffer)
		binary.Write(buf2, binary.BigEndian, w.engineTime)
		iv := string(buf.Bytes()) + string(buf2.Bytes()) + privParam

		// Decrypt
		decrypted := make([]byte, len(payload))
		err := decryptAESCFB(decrypted, []byte(payload), []byte(w.privKey), []byte(iv))
		if err != nil {
			return "", err
		}
		return string(decrypted), nil
	}

	desKey := w.privKey[:8]
	preIV := w.privKey[8:16]
	iv := strXor(preIV, privParam)

	//DES Decrypt
	plen := len(payload)
	if (plen % 8) != 0 {
		panic("DES encrypted payload is not multiple of 8 bytes\n")
	}
	decrypted := make([]byte, len(payload))
	decryptDESCBC(decrypted, []byte(payload), []byte(desKey), []byte(iv))
	return string(decrypted), nil
}

// GetNextV3 issues a GETNEXT SNMPv3 request.
func (w *SNMP) GetNextV3(oid Oid) (*Oid, interface{}, error) {
	return w.doGetV3(oid, AsnGetNextRequest)
}

// GetV3 sends an SNMPv3 get request requesting the value for an oid.
func (w *SNMP) GetV3(oid Oid) (interface{}, error) {
	_, val, err := w.doGetV3(oid, AsnGetRequest)
	return val, err
}

// A function does both GetNext and Get for SNMP V3
func (w *SNMP) doGetV3(oid Oid, request BERType) (*Oid, interface{}, error) {
	msgID := getRandomRequestID()
	requestID := getRandomRequestID()
	req, err := EncodeSequence(
		[]interface{}{Sequence, w.engineID, "",
			[]interface{}{request, requestID, 0, 0,
				[]interface{}{Sequence,
					[]interface{}{Sequence, oid, nil}}}})
	if err != nil {
		panic(err)
	}

	encrypted, privParam, _ := w.encrypt(string(req))

	v3Header, err := EncodeSequence([]interface{}{Sequence, w.engineID,
		int(w.engineBoots), int(w.engineTime), w.user, strings.Repeat("\x00", 12), privParam})
	if err != nil {
		panic(err)
	}

	flags := string([]byte{7})
	USM := 0x03
	packet, err := EncodeSequence([]interface{}{
		Sequence, int(w.Version),
		[]interface{}{Sequence, msgID, maxMsgSize, flags, USM},
		string(v3Header),
		encrypted})
	if err != nil {
		panic(err)
	}
	authParam := w.auth(string(packet))
	finalPacket := strings.Replace(string(packet), strings.Repeat("\x00", 12), authParam, 1)

	response := make([]byte, bufSize)
	numRead, err := poll(w.conn, []byte(finalPacket), response, w.retries, 500*time.Millisecond)
	if err != nil {
		return nil, nil, err
	}

	decodedResponse, err := DecodeSequence(response[:numRead])
	if err != nil {
		fmt.Printf("Error decoding getNext:%v\n", err)
		return nil, nil, err
	}
	/*
		for i, val := range decodedResponse{
			fmt.Printf("Resp:%v:type=%v\n",i,reflect.TypeOf(val));
		}
	*/

	v3HeaderStr := decodedResponse[3].(string)
	v3HeaderDecoded, err := DecodeSequence([]byte(v3HeaderStr))
	if err != nil {
		fmt.Printf("Error 2 decoding:%v\n", err)
		return nil, nil, err
	}

	w.engineID = v3HeaderDecoded[1].(string)
	w.engineBoots = int32(v3HeaderDecoded[2].(int))
	w.engineTime = int32(v3HeaderDecoded[3].(int))
	// skip checking authParam for now
	respAuthParam := v3HeaderDecoded[5].(string)
	respPrivParam := v3HeaderDecoded[6].(string)

	if len(respAuthParam) == 0 || len(respPrivParam) == 0 {
		return nil, nil, fmt.Errorf("Error,response is not encrypted.")
	}

	encryptedResp := decodedResponse[4].(string)
	plainResp, _ := w.decrypt(encryptedResp, respPrivParam)

	pduDecoded, err := DecodeSequence([]byte(plainResp))
	if err != nil {
		fmt.Printf("Error 3 decoding:%v\n", err)
		return nil, nil, err
	}

	// Find the varbinds
	respPacket := pduDecoded[3].([]interface{})
	varbinds := respPacket[4].([]interface{})
	result := varbinds[1].([]interface{})

	resultOid := result[1].(Oid)
	resultVal := result[2]

	return &resultOid, resultVal, nil
}

// GetNext issues a GETNEXT SNMP request.
func (w SNMP) GetNext(oid Oid) (*Oid, interface{}, error) {
	requestID := getRandomRequestID()
	req, err := EncodeSequence([]interface{}{Sequence, int(w.Version), w.Community,
		[]interface{}{AsnGetNextRequest, requestID, 0, 0,
			[]interface{}{Sequence,
				[]interface{}{Sequence, oid, nil}}}})
	if err != nil {
		return nil, nil, err
	}

	response := make([]byte, bufSize)
	numRead, err := poll(w.conn, req, response, w.retries, 500*time.Millisecond)
	if err != nil {
		return nil, nil, err
	}

	decodedResponse, err := DecodeSequence(response[:numRead])
	if err != nil {
		return nil, nil, err
	}

	// Find the varbinds
	respPacket := decodedResponse[3].([]interface{})
	varbinds := respPacket[4].([]interface{})
	result := varbinds[1].([]interface{})

	resultOid := result[1].(Oid)
	resultVal := result[2]

	return &resultOid, resultVal, nil
}

// GetBulk is semantically the same as maxRepetitions getnext requests, but in a single GETBULK SNMP packet.
// Caveat: many devices will silently drop GETBULK requests for more than some number of maxrepetitions, if
// it doesn't work, try with a lower value and/or use GetTable.
func (w SNMP) GetBulk(oid Oid, maxRepetitions int) (map[string]interface{}, error) {
	requestID := getRandomRequestID()
	req, err := EncodeSequence([]interface{}{Sequence, int(w.Version), w.Community,
		[]interface{}{AsnGetBulkRequest, requestID, 0, maxRepetitions,
			[]interface{}{Sequence,
				[]interface{}{Sequence, oid, nil}}}})
	if err != nil {
		return nil, err
	}

	response := make([]byte, bufSize, bufSize)
	numRead, err := poll(w.conn, req, response, w.retries, 500*time.Millisecond)
	if err != nil {
		return nil, err
	}

	decodedResponse, err := DecodeSequence(response[:numRead])
	if err != nil {
		return nil, err
	}

	// Find the varbinds
	respPacket := decodedResponse[3].([]interface{})
	respVarbinds := respPacket[4].([]interface{})

	result := make(map[string]interface{})
	for _, v := range respVarbinds[1:] { // First element is just a sequence
		oid := v.([]interface{})[1].(Oid).String()
		value := v.([]interface{})[2]
		result[oid] = value
	}

	return result, nil
}

// GetTable efficiently gets an entire table from an SNMP agent. Uses GETBULK requests to go fast.
func (w SNMP) GetTable(oid Oid) (map[string]interface{}, error) {
	result := make(map[string]interface{})
	lastOid := oid.Copy()
	for lastOid.Within(oid) {
		log.Printf("Sending GETBULK(%v, 50)\n", lastOid)
		results, err := w.GetBulk(lastOid, 50)
		if err != nil {
			return nil, fmt.Errorf("received GetBulk error => %v\n", err)
		}
		newLastOid := lastOid.Copy()
		for o, v := range results {
			oAsOid := MustParseOid(o)
			if oAsOid.Within(oid) {
				result[o] = v
			}
			newLastOid = oAsOid
		}

		if reflect.DeepEqual(lastOid, newLastOid) {
			// Not making any progress ? Assume we reached end of table.
			break
		}
		lastOid = newLastOid
	}
	return result, nil
}

// Trap object.
type Trap struct {
	Version   int
	OID       Oid
	Community string
	Username  string
	Address   string
	VarBinds  map[string]interface{}
}

// ParseTrap parses a received SNMP trap and returns  a map of oid to objects
func (w SNMP) ParseTrap(response []byte) (Trap, error) {
	t := Trap{VarBinds: map[string]interface{}{}}

	decodedResponse, err := DecodeSequence(response)
	if err != nil {
		return t, err
	}

	// Fetch the varbinds out of the packet.
	t.Version = decodedResponse[1].(int)
	if t.Version <= 1 {
		t.Version++
	}

	if t.Version < 3 {
		t.Community = decodedResponse[2].(string)
	} else {
		/*
			for i, val := range decodedResponse{
				fmt.Printf("Resp:%v:type=%v\n",i,reflect.TypeOf(val));
			}
		*/
		v3HeaderStr := decodedResponse[3].(string)
		v3HeaderDecoded, err := DecodeSequence([]byte(v3HeaderStr))
		if err != nil {
			return t, err
		}

		w.engineID = v3HeaderDecoded[1].(string)
		w.engineBoots = int32(v3HeaderDecoded[2].(int))
		w.engineTime = int32(v3HeaderDecoded[3].(int))
		w.user = v3HeaderDecoded[4].(string)
		respAuthParam := v3HeaderDecoded[5].(string)
		respPrivParam := v3HeaderDecoded[6].(string)

		if len(respAuthParam) == 0 || len(respPrivParam) == 0 {
			return t, errors.New("response is not encrypted")
		}
		if len(w.TrapUsers) == 0 {
			return t, errors.New("No SNMP V3 trap user configured")
		}

		founduser := false
		for _, v3user := range w.TrapUsers {
			if v3user.User == w.user {
				w.authAlg = v3user.AuthAlg
				w.privAlg = v3user.PrivAlg
				w.authPwd = v3user.AuthPwd
				w.privPwd = v3user.PrivPwd
				founduser = true
				break
			}
		}
		if !founduser {
			return t, errors.New("No matching user found")
		}

		t.Username = w.user

		//keys
		w.authKey = passwordToKey(w.authPwd, w.engineID, w.authAlg)
		privKey := passwordToKey(w.privPwd, w.engineID, w.authAlg)
		w.privKey = string(([]byte(privKey))[0:16])

		encryptedResp := decodedResponse[4].(string)
		plainResp, _ := w.decrypt(encryptedResp, respPrivParam)

		pduDecoded, err := DecodeSequence([]byte(plainResp))
		if err != nil {
			return t, err
		}
		decodedResponse = pduDecoded
	}
	//fmt.Printf("%#v\n",decodedResponse);

	respPacket := decodedResponse[3].([]interface{})
	var varbinds []interface{}
	if t.Version == 1 {
		t.OID, _ = respPacket[1].(Oid)
		t.Address, _ = respPacket[2].(string)
		//fmt.Printf("Generic Trap: %d\n", respPacket[3])
		varbinds = respPacket[6].([]interface{})
	} else {
		varbinds = respPacket[4].([]interface{})
	}

	for i := 1; i < len(varbinds); i++ {
		varoid := varbinds[i].([]interface{})[1]
		result := varbinds[i].([]interface{})[2]
		t.VarBinds[varoid.(Oid).String()] = result
	}

	return t, nil
}

// Close the net.conn in SNMP.
func (w SNMP) Close() error {
	return w.conn.Close()
}
