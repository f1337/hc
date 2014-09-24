package gohap

import(
    "github.com/brutella/gohap"
    
    "io"
    "fmt"
    "encoding/hex"
    "bytes"
)

const (
    SequenceWaitingForRequest  = 0x00
    SequenceStartRequest       = 0x01
    SequenceStartRespond       = 0x02
    SequenceVerifyRequest      = 0x03
    SequenceVerifyRespond      = 0x04
    SequenceKeyExchangeRequest = 0x05
    SequenceKeyExchangeRepond  = 0x06
)

type SetupController struct {
    context *gohap.Context
    accessory *gohap.Accessory
    session *PairSetupSession
    curSeq byte
}

func NewSetupController(context *gohap.Context, accessory *gohap.Accessory) (*SetupController, error) {
    
    session, err := NewPairSetupSession("Pair-Setup", accessory.Password)
    if err != nil {
        return nil, err
    }
    
    controller := SetupController{
                                    context: context,
                                    accessory: accessory,
                                    session: session,
                                    curSeq: SequenceWaitingForRequest,
                                }
    
    return &controller, nil
}

func (c *SetupController) Handle(r io.Reader) (io.Reader, error) {
    var tlv_out *gohap.TLV8Container
    var err error
    
    tlv_in, err := gohap.ReadTLV8(r)
    if err != nil {
        return nil, err
    }
    
    method := tlv_in.GetByte(gohap.TLVType_AuthMethod)
    
    // It is valid that method is not sent
    // If method is sent then it must be 0x00
    if method != 0x00 {
        return nil, gohap.NewErrorf("Cannot handle auth method %b", method)
    }
    
    seq := tlv_in.GetByte(gohap.TLVType_SequenceNumber)
    fmt.Println("->     Seq:", seq)
    
    switch seq {
    case SequenceStartRequest:
        if c.curSeq != SequenceWaitingForRequest {
            c.reset()
            return nil, gohap.NewErrorf("Controller is in wrong state (%d)", c.curSeq)
        }
        
        tlv_out, err = c.handlePairStart(tlv_in)
    case SequenceVerifyRequest:
        if c.curSeq != SequenceStartRespond {
            c.reset()
            return nil, gohap.NewErrorf("Controller is in wrong state (%d)", c.curSeq)
        }
        
        tlv_out, err = c.handlePairVerify(tlv_in)
    case SequenceKeyExchangeRequest:        
        if c.curSeq != SequenceVerifyRespond {
            c.reset()
            return nil, gohap.NewErrorf("Controller is in wrong state (%d)", c.curSeq)
        }
        
        tlv_out, err = c.handleKeyExchange(tlv_in)
    default:
        return nil, gohap.NewErrorf("Cannot handle sequence number %d", seq)
    }
    
    if err != nil {
        fmt.Println("[ERROR]", err)
        return nil, err
    } else {
        fmt.Println("<-     Seq:", tlv_out.GetByte(gohap.TLVType_SequenceNumber))
        fmt.Println("-------------")
        return tlv_out.BytesBuffer(), nil
    }
}

// Client -> Server
// - Auth start
//
// Server -> Client
// - B: server public key
// - s: salt
func (c *SetupController) handlePairStart(tlv_in *gohap.TLV8Container) (*gohap.TLV8Container, error) {
    tlv_out := gohap.TLV8Container{}
    c.curSeq = SequenceStartRespond
    
    tlv_out.SetByte(gohap.TLVType_SequenceNumber, c.curSeq)
    tlv_out.SetBytes(gohap.TLVType_PublicKey, c.session.publicKey)
    tlv_out.SetBytes(gohap.TLVType_Salt, c.session.salt)
    
    fmt.Println("<-     B:", hex.EncodeToString(tlv_out.GetBytes(gohap.TLVType_PublicKey)))
    fmt.Println("<-     s:", hex.EncodeToString(tlv_out.GetBytes(gohap.TLVType_Salt)))
    
    return &tlv_out, nil
}

// Client -> Server
// - A: client public key
// - M1: proof
// 
// Server -> client
// - M2: proof
// or
// - auth error
func (c *SetupController) handlePairVerify(tlv_in *gohap.TLV8Container) (*gohap.TLV8Container, error) {
    tlv_out := gohap.TLV8Container{}
    c.curSeq = SequenceVerifyRespond
    
    tlv_out.SetByte(gohap.TLVType_SequenceNumber, c.curSeq)
    
    cpublicKey := tlv_in.GetBytes(gohap.TLVType_PublicKey)
    fmt.Println("->     A:", hex.EncodeToString(cpublicKey))
    
    err := c.session.SetupSecretKeyFromClientPublicKey(cpublicKey)
    if err != nil {
        return nil, err
    }
    
    cproof := tlv_in.GetBytes(gohap.TLVType_Proof)
    fmt.Println("->     M1:", hex.EncodeToString(cproof))
    
    sproof, err := c.session.ProofFromClientProof(cproof)
    if err != nil || len(sproof) == 0 { // proof `M1` is wrong
        fmt.Println("[Failed] Proof M1 is wrong")
        c.reset()
        tlv_out.SetByte(gohap.TLVType_ErrorCode, gohap.TLVStatus_AuthError) // return error 2
    } else {
        fmt.Println("[Success] Proof M1 is valid")
        err := c.session.SetupEncryptionKey([]byte("Pair-Setup-Encrypt-Salt"), []byte("Pair-Setup-Encrypt-Info"))
        if err != nil {
            return nil, err
        }
        
        // Return proof `M1`
        tlv_out.SetBytes(gohap.TLVType_Proof, sproof)
    }
    
    fmt.Println("<-     M2:", hex.EncodeToString(tlv_out.GetBytes(gohap.TLVType_Proof)))
    fmt.Println("        S:", hex.EncodeToString(c.session.secretKey))
    fmt.Println("        K:", hex.EncodeToString(c.session.encryptionKey[:]))
    
    return &tlv_out, nil
}

// Client -> Server
// - encrypted tlv8: client LTPK, client name and signature (of H, client name, LTPK)
// - auth tag (mac)
// 
// Server
// - Validate signature of encrpyted tlv8
// - Read and store client LTPK and name
// 
// Server -> Client
// - encrpyted tlv8: accessory LTPK, accessory name, signature (of H2, accessory name, LTPK)
func (c *SetupController) handleKeyExchange(tlv_in *gohap.TLV8Container) (*gohap.TLV8Container, error) {
    tlv_out := gohap.TLV8Container{}
    
    c.curSeq = SequenceKeyExchangeRepond
    
    tlv_out.SetByte(gohap.TLVType_SequenceNumber, c.curSeq)
    
    data := tlv_in.GetBytes(gohap.TLVType_EncryptedData)    
    message := data[:(len(data) - 16)]
    var mac [16]byte
    copy(mac[:], data[len(message):]) // 16 byte (MAC)
    fmt.Println("->     Message:", hex.EncodeToString(message))
    fmt.Println("->     MAC:", hex.EncodeToString(mac[:]))
    
    decrypted, err := gohap.Chacha20DecryptAndPoly1305Verify(c.session.encryptionKey[:], []byte("PS-Msg05"), message, mac, nil)
    
    if err != nil {
        c.reset()
        fmt.Println(err)
        tlv_out.SetByte(gohap.TLVType_ErrorCode, gohap.TLVStatus_UnkownError) // return error 1
    } else {
        decrypted_buffer := bytes.NewBuffer(decrypted)
        tlv_in, err := gohap.ReadTLV8(decrypted_buffer)
        if err != nil {
            return nil, err
        }
        
        username  := tlv_in.GetString(gohap.TLVType_Username)
        ltpk      := tlv_in.GetBytes(gohap.TLVType_PublicKey)
        signature := tlv_in.GetBytes(gohap.TLVType_Ed25519Signature)
        fmt.Println("->     Username:", username)
        fmt.Println("->     LTPK:", hex.EncodeToString(ltpk))
        fmt.Println("->     Signature:", hex.EncodeToString(signature))
        
        // Calculate `H`
        H, _ := gohap.HKDF_SHA512(c.session.secretKey, []byte("Pair-Setup-Controller-Sign-Salt"), []byte("Pair-Setup-Controller-Sign-Info"))
        material := make([]byte, 0)
        material = append(material, H[:]...)
        material = append(material, []byte(username)...)
        material = append(material, ltpk...)
        
        if gohap.ValidateED25519Signature(ltpk, material, signature) == false {
            fmt.Println("[Failed] ed25519 signature is invalid")
            c.reset()
            tlv_out.SetByte(gohap.TLVType_ErrorCode, gohap.TLVStatus_AuthError) // return error 2
        } else {
            fmt.Println("[Success] ed25519 signature is valid")
            // Store client LTPK and name
            client := gohap.NewClient(username, ltpk)
            c.context.SaveClient(client)
            fmt.Printf("[Storage] Stored LTPK '%s' for client '%s'\n", hex.EncodeToString(ltpk), username)
            
            // Send username, LTPK, signature as encrypted message
            H2, err := gohap.HKDF_SHA512(c.session.secretKey, []byte("Pair-Setup-Accessory-Sign-Salt"), []byte("Pair-Setup-Accessory-Sign-Info"))
            material = make([]byte, 0)
            material = append(material, H2[:]...)
            material = append(material, []byte(c.accessory.Name)...)
            material = append(material, c.accessory.PublicKey...)

            signature, err := gohap.ED25519Signature(c.accessory.SecretKey, material)
            if err != nil {
                return nil, err
            }
            
            tlvPairKeyExchange := gohap.TLV8Container{}
            tlvPairKeyExchange.SetString(gohap.TLVType_Username, c.accessory.Name)
            tlvPairKeyExchange.SetBytes(gohap.TLVType_PublicKey, c.accessory.PublicKey)
            tlvPairKeyExchange.SetBytes(gohap.TLVType_Ed25519Signature, []byte(signature))
            
            fmt.Println("<-     Username:", tlvPairKeyExchange.GetString(gohap.TLVType_Username))
            fmt.Println("<-     LTPK:", hex.EncodeToString(tlvPairKeyExchange.GetBytes(gohap.TLVType_PublicKey)))
            fmt.Println("<-     Signature:", hex.EncodeToString(tlvPairKeyExchange.GetBytes(gohap.TLVType_Ed25519Signature)))
            
            encrypted, mac, _ := gohap.Chacha20EncryptAndPoly1305Seal(c.session.encryptionKey[:], []byte("PS-Msg06"), tlvPairKeyExchange.BytesBuffer().Bytes(), nil)    
            tlv_out.SetByte(gohap.TLVType_AuthMethod, 0)
            tlv_out.SetByte(gohap.TLVType_SequenceNumber, SequenceKeyExchangeRequest)
            tlv_out.SetBytes(gohap.TLVType_EncryptedData, append(encrypted, mac[:]...))
        }
    }
    
    return &tlv_out, nil
}

func (c *SetupController) reset() {
    c.curSeq = SequenceWaitingForRequest
    // TODO: reset session
}