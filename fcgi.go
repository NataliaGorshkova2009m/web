package web

import (
    "bytes"
    "bufio"
    "encoding/binary"
    "fmt"
    "http"
    "io"
    "log"
    "net"
    "os"
)

const (
    FcgiBeginRequest = iota + 1
    FcgiAbortRequest
    FcgiEndRequest
    FcgiParams
    FcgiStdin
    FcgiStdout
    FcgiStderr
    FcgiData
    FcgiGetValues
    FcgiGetValuesResult
    FcgiUnknownType
    FcgiMaxType = FcgiUnknownType
)

const (
    FcgiRequestComplete = iota
    FcgiCantMpxConn
    FcgiOverloaded
    FcgiUnknownRole
)

type fcgiHeader struct {
    Version       uint8
    Type          uint8
    RequestId     uint16
    ContentLength uint16
    PaddingLength uint8
    Reserved      uint8
}

func (h fcgiHeader) bytes() []byte {
    order := binary.BigEndian
    buf := make([]byte, 8)
    buf[0] = h.Version
    buf[1] = h.Type
    order.PutUint16(buf[2:4], h.RequestId)
    order.PutUint16(buf[4:6], h.ContentLength)
    buf[6] = h.PaddingLength
    buf[7] = h.Reserved
    return buf
}

func newFcgiRecord(typ int, requestId int, data []byte) []byte {
    var record bytes.Buffer
    l := len(data)
    // round to the nearest 8
    padding := make([]byte, uint8(-l&7))
    hdr := fcgiHeader{
        Version: 1,
        Type: uint8(typ),
        RequestId: uint16(requestId),
        ContentLength: uint16(l),
        PaddingLength: uint8(len(padding)),
    }

    //write the header
    record.Write(hdr.bytes())
    record.Write(data)
    record.Write(padding)

    return record.Bytes()
}

type fcgiEndRequest struct {
    appStatus      uint32
    protocolStatus uint8
    reserved       [3]uint8
}

func (er fcgiEndRequest) bytes() []byte {
    buf := make([]byte, 8)
    binary.BigEndian.PutUint32(buf, er.appStatus)
    buf[4] = er.protocolStatus
    return buf
}

type fcgiConn struct {
    requestId    uint16
    fd           io.ReadWriteCloser
    headers      map[string]string
    wroteHeaders bool
}

func (conn *fcgiConn) fcgiWrite(data []byte) (err os.Error) {
    l := len(data)
    // round to the nearest 8
    padding := make([]byte, uint8(-l&7))
    hdr := fcgiHeader{
        Version: 1,
        Type: FcgiStdout,
        RequestId: conn.requestId,
        ContentLength: uint16(l),
        PaddingLength: uint8(len(padding)),
    }

    //write the header
    hdrBytes := hdr.bytes()
    _, err = conn.fd.Write(hdrBytes)

    if err != nil {
        return err
    }

    _, err = conn.fd.Write(data)
    if err != nil {
        return err
    }

    _, err = conn.fd.Write(padding)
    if err != nil {
        return err
    }

    return err
}

func (conn *fcgiConn) Write(data []byte) (n int, err os.Error) {
    var buf bytes.Buffer
    if !conn.wroteHeaders {
        conn.wroteHeaders = true
        for k, v := range conn.headers {
            buf.WriteString(k + ": " + v + "\r\n")
        }
        buf.WriteString("\r\n")
        conn.fcgiWrite(buf.Bytes())
    }

    err = conn.fcgiWrite(data)

    if err != nil {
        return 0, err
    }

    return len(data), nil
}

func (conn *fcgiConn) WriteString(data string) {
    var buf bytes.Buffer
    buf.WriteString(data)
    conn.Write(buf.Bytes())
}

func (conn *fcgiConn) StartResponse(status int) {
    var buf bytes.Buffer
    text := statusText[status]
    fmt.Fprintf(&buf, "HTTP/1.1 %d %s\r\n", status, text)
    conn.fcgiWrite(buf.Bytes())
}

func (conn *fcgiConn) SetHeader(hdr string, val string) {
    conn.headers[hdr] = val
}

func (conn *fcgiConn) complete() {
    content := fcgiEndRequest{appStatus: 200, protocolStatus: FcgiRequestComplete}.bytes()
    l := len(content)

    hdr := fcgiHeader{
        Version: 1,
        Type: FcgiEndRequest,
        RequestId: uint16(conn.requestId),
        ContentLength: uint16(l),
        PaddingLength: 0,
    }

    conn.fd.Write(hdr.bytes())
    conn.fd.Write(content)
}

func (conn *fcgiConn) Close() {}

func readFcgiParams(data []byte) map[string]string {
    var params = make(map[string]string)

    for idx := 0; len(data) > idx; {
        var keySize int = int(data[idx])
        if keySize>>7 == 0 {
            idx += 1
        } else {
            binary.Read(bytes.NewBuffer(data[idx:idx+4]), binary.BigEndian, &keySize)
            idx += 4
        }

        var valSize int = int(data[idx])
        if valSize>>7 == 0 {
            idx += 1
        } else {
            binary.Read(bytes.NewBuffer(data[idx:idx+4]), binary.BigEndian, &valSize)
            idx += 4
        }

        key := data[idx : idx+keySize]
        idx += keySize
        val := data[idx : idx+valSize]
        idx += valSize
        params[string(key)] = string(val)
    }

    return params
}

func buildRequest(headers map[string]string) *Request {
    method, _ := headers["REQUEST_METHOD"]
    host, _ := headers["HTTP_HOST"]
    path, _ := headers["REQUEST_URI"]
    port, _ := headers["SERVER_PORT"]
    proto, _ := headers["SERVER_PROTOCOL"]
    rawurl := "http://" + host + ":" + port + path

    url, _ := http.ParseURL(rawurl)
    useragent, _ := headers["USER_AGENT"]

    httpheader := map[string]string{}
    if method == "POST" {
        if ctype, ok := headers["CONTENT_TYPE"]; ok {
            httpheader["Content-Type"] = ctype
        }

        if clength, ok := headers["CONTENT_LENGTH"]; ok {
            httpheader["Content-Length"] = clength
        }
    }

    req := Request{Method: method,
        RawURL: rawurl,
        URL: url,
        Proto: proto,
        Host: host,
        UserAgent: useragent,
        Header: httpheader,
    }

    return &req
}

func handleFcgiConnection(fd io.ReadWriteCloser) {
    br := bufio.NewReader(fd)
    var req *Request
    var fc *fcgiConn
    var body bytes.Buffer
    for {
        var h fcgiHeader
        err := binary.Read(br, binary.BigEndian, &h)
        if err == os.EOF {
            break
        }
        if err != nil {
            log.Stderrf("FCGI Error", err.String())
            break
        }
        content := make([]byte, h.ContentLength)
        br.Read(content)

        //read padding
        if h.PaddingLength > 0 {
            padding := make([]byte, h.PaddingLength)
            br.Read(padding)
        }

        switch h.Type {
        case FcgiBeginRequest:
            fc = &fcgiConn{h.RequestId, fd, make(map[string]string), false}
        case FcgiParams:
            if h.ContentLength > 0 {
                params := readFcgiParams(content)
                req = buildRequest(params)
            }
        case FcgiStdin:
            if h.ContentLength > 0 {
                body.Write(content)
            } else if h.ContentLength == 0 {
                req.Body = &body
                routeHandler(req, fc)
                fc.complete()
            }
        case FcgiData:
            if h.ContentLength > 0 {
                body.Write(content)
            }
        case FcgiAbortRequest:
        }
    }
}

func listenAndServeFcgi(addr string) {
    l, err := net.Listen("tcp", addr)
    if err != nil {
        log.Stderrf("FCGI listen error", err.String())
        return
    }

    for {
        fd, err := l.Accept()
        if err != nil {
            log.Stderrf("FCGI accept error", err.String())
            break
        }
        go handleFcgiConnection(fd)
    }
}
