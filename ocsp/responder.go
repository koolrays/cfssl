package ocsp

import (
	"encoding/base64"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"

	"github.com/cloudflare/cfssl/log"
	"golang.org/x/crypto/ocsp"
)

var (
	malformedRequestErrorResponse = []byte{0x30, 0x03, 0x0A, 0x01, 0x01}
	internalErrorErrorResponse    = []byte{0x30, 0x03, 0x0A, 0x01, 0x02}
	tryLaterErrorResponse         = []byte{0x30, 0x03, 0x0A, 0x01, 0x03}
	sigRequredErrorResponse       = []byte{0x30, 0x03, 0x0A, 0x01, 0x05}
	unauthorizedErrorResponse     = []byte{0x30, 0x03, 0x0A, 0x01, 0x06}
)

// Source represents the logical source of OCSP responses, i.e.,
// the logic that actually chooses a response based on a request.  In
// order to create an actual responder, wrap one of these in a Responder
// object and pass it to http.Handle.
type Source interface {
	Response(*ocsp.Request) ([]byte, bool)
}

// An InMemorySource is a map from serialNumber -> der(response)
type InMemorySource map[string][]byte

// Response looks up an OCSP response to provide for a given request.
// InMemorySource looks up a response purely based on serial number,
// without regard to what issuer the request is asking for.
func (src InMemorySource) Response(request *ocsp.Request) (response []byte, present bool) {
	response, present = src[request.SerialNumber.String()]
	return
}

// NewSourceFromFile reads the named file into an InMemorySource.
// The file read by this function must contain whitespace-separated OCSP
// responses. Each OCSP response must be in base64-encoded DER form (i.e.,
// PEM without headers or whitespace).  Invalid responses are ignored.
// This function pulls the entire file into an InMemorySource.
func NewSourceFromFile(responseFile string) (Source, error) {
	fileContents, err := ioutil.ReadFile(responseFile)
	if err != nil {
		return nil, err
	}

	responsesB64 := regexp.MustCompile("\\s").Split(string(fileContents), -1)
	src := InMemorySource{}
	for _, b64 := range responsesB64 {
		// if the line/space is empty just skip
		if b64 == "" {
			continue
		}
		der, tmpErr := base64.StdEncoding.DecodeString(b64)
		if tmpErr != nil {
			log.Errorf("Base64 decode error on: %s", b64)
			continue
		}

		response, tmpErr := ocsp.ParseResponse(der, nil)
		if tmpErr != nil {
			log.Errorf("OCSP decode error on: %s", b64)
			continue
		}

		src[response.SerialNumber.String()] = der
	}

	log.Infof("Read %d OCSP responses", len(src))
	return src, nil
}

// A Responder object provides the HTTP logic to expose a
// Source of OCSP responses.
type Responder struct {
	Source Source
}

// A Responder can process both GET and POST requests.  The mapping
// from an OCSP request to an OCSP response is done by the Source;
// the Responder simply decodes the request, and passes back whatever
// response is provided by the source.
// Note: The caller must use http.StripPrefix to strip any path components
// (including '/') on GET requests.
// Do not use this responder in conjunction with http.NewServeMux, because the
// default handler will try to canonicalize path components by changing any
// strings of repeated '/' into a single '/', which will break the base64
// encoding.
func (rs Responder) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	// Read response from request
	var requestBody []byte
	var err error
	switch request.Method {
	case "GET":
		base64Request, err := url.QueryUnescape(request.URL.Path)
		if err != nil {
			log.Errorf("Error decoding URL: %s", request.URL.Path)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		// url.QueryUnescape not only unescapes %2B escaping, but it additionally
		// turns the resulting '+' into a space, which makes base64 decoding fail.
		// So we go back afterwards and turn ' ' back into '+'. This means we
		// accept some malformed input that includes ' ' or %20, but that's fine.
		base64RequestBytes := []byte(base64Request)
		for i := range base64RequestBytes {
			if base64RequestBytes[i] == ' ' {
				base64RequestBytes[i] = '+'
			}
		}
		requestBody, err = base64.StdEncoding.DecodeString(string(base64RequestBytes))
		if err != nil {
			log.Errorf("Error decoding base64 from URL: %s", base64Request)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
	case "POST":
		requestBody, err = ioutil.ReadAll(request.Body)
		if err != nil {
			log.Errorf("Problem reading body of POST: %s", err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
	default:
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// TODO log request
	b64Body := base64.StdEncoding.EncodeToString(requestBody)
	log.Infof("Received OCSP request: %s", b64Body)

	// All responses after this point will be OCSP.
	// We could check for the content type of the request, but that
	// seems unnecessariliy restrictive.
	response.Header().Add("Content-Type", "application/ocsp-response")

	// Parse response as an OCSP request
	// XXX: This fails if the request contains the nonce extension.
	//      We don't intend to support nonces anyway, but maybe we
	//      should return unauthorizedRequest instead of malformed.
	ocspRequest, err := ocsp.ParseRequest(requestBody)
	if err != nil {
		log.Errorf("Error decoding request body: %s", b64Body)
		response.WriteHeader(http.StatusBadRequest)
		response.Write(malformedRequestErrorResponse)
		return
	}

	// Look up OCSP response from source
	ocspResponse, found := rs.Source.Response(ocspRequest)
	if !found {
		log.Errorf("No response found for request: %s", b64Body)
		response.Write(unauthorizedErrorResponse)
		return
	}

	// Write OCSP response to response
	response.WriteHeader(http.StatusOK)
	response.Write(ocspResponse)
}
