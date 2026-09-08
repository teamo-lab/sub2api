package service

import "github.com/gin-gonic/gin"

// The first complete failure successfully written to the client owns Ops
// attribution. Frames read later while draining cannot change that snapshot.
type openAIStreamErrorOrigin struct {
	service     *OpenAIGatewayService
	c           *gin.Context
	account     *Account
	passthrough bool
	requestID   string
	payload     []byte
	message     string
	writeOK     bool
	sealed      bool
	committed   bool
	skip        bool
}

type openAIStreamFinalError struct {
	status int
	skip   bool
}

func (s *OpenAIGatewayService) newOpenAIStreamErrorOrigin(c *gin.Context, account *Account, passthrough bool, requestID string) *openAIStreamErrorOrigin {
	return &openAIStreamErrorOrigin{service: s, c: c, account: account, passthrough: passthrough, requestID: requestID}
}

func (o *openAIStreamErrorOrigin) stage(payload []byte, message string) {
	if o.sealed {
		return
	}
	o.payload = append(o.payload[:0], payload...)
	o.message = message
	o.writeOK = true
}

func (o *openAIStreamErrorOrigin) observeWrite(written, expected int, err error) {
	if !o.committed && len(o.payload) > 0 && (err != nil || written != expected) {
		o.writeOK = false
	}
}

// Seal when a complete failure has entered the downstream pending buffer.
// A later drain-only frame must not replace it while the flush is outstanding.
func (o *openAIStreamErrorOrigin) seal() {
	if o.sealed || !o.writeOK || len(o.payload) == 0 {
		return
	}
	o.sealed = true
	o.skip = currentOpsFailureSkipMonitoring(o.c)
}

func (o *openAIStreamErrorOrigin) commit() {
	if o.sealed && o.writeOK {
		o.committed = true
	}
}

func (o *openAIStreamErrorOrigin) record() {
	if !o.committed {
		return
	}
	o.c.Set(OpsSkipPassthroughKey, o.skip)
	o.service.recordOpenAIStreamUpstreamError(o.c, o.account, o.passthrough, o.requestID, "http_error", o.payload, o.message,
		openAIStreamFinalError{status: openAIStreamFailedEventSemanticStatus(o.payload, o.message), skip: o.skip})
}
