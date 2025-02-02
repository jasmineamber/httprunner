package hrp

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"

	"github.com/httprunner/httprunner/hrp/internal/builtin"
	"github.com/httprunner/httprunner/hrp/internal/json"
)

type HTTPMethod string

const (
	httpGET     HTTPMethod = "GET"
	httpHEAD    HTTPMethod = "HEAD"
	httpPOST    HTTPMethod = "POST"
	httpPUT     HTTPMethod = "PUT"
	httpDELETE  HTTPMethod = "DELETE"
	httpOPTIONS HTTPMethod = "OPTIONS"
	httpPATCH   HTTPMethod = "PATCH"
)

// Request represents HTTP request data structure.
// This is used for teststep.
type Request struct {
	Method         HTTPMethod             `json:"method" yaml:"method"` // required
	URL            string                 `json:"url" yaml:"url"`       // required
	Params         map[string]interface{} `json:"params,omitempty" yaml:"params,omitempty"`
	Headers        map[string]string      `json:"headers,omitempty" yaml:"headers,omitempty"`
	Cookies        map[string]string      `json:"cookies,omitempty" yaml:"cookies,omitempty"`
	Body           interface{}            `json:"body,omitempty" yaml:"body,omitempty"`
	Json           interface{}            `json:"json,omitempty" yaml:"json,omitempty"`
	Data           interface{}            `json:"data,omitempty" yaml:"data,omitempty"`
	Timeout        float32                `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	AllowRedirects bool                   `json:"allow_redirects,omitempty" yaml:"allow_redirects,omitempty"`
	Verify         bool                   `json:"verify,omitempty" yaml:"verify,omitempty"`
}

func newRequestBuilder(parser *Parser, config *TConfig, stepRequest *Request) *requestBuilder {
	// convert request struct to map
	jsonRequest, _ := json.Marshal(stepRequest)
	var requestMap map[string]interface{}
	_ = json.Unmarshal(jsonRequest, &requestMap)

	return &requestBuilder{
		stepRequest: stepRequest,
		req: &http.Request{
			Header:     make(http.Header),
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
		},
		config:     config,
		parser:     parser,
		requestMap: requestMap,
	}
}

type requestBuilder struct {
	stepRequest *Request
	req         *http.Request
	parser      *Parser
	config      *TConfig
	requestMap  map[string]interface{}
}

func (r *requestBuilder) prepareHeaders(stepVariables map[string]interface{}) error {
	// prepare request headers
	stepHeaders := r.stepRequest.Headers
	if r.config.Headers != nil {
		// override headers
		stepHeaders = mergeMap(stepHeaders, r.config.Headers)
	}

	if len(stepHeaders) > 0 {
		headers, err := r.parser.ParseHeaders(stepHeaders, stepVariables)
		if err != nil {
			return errors.Wrap(err, "parse headers failed")
		}
		for key, value := range headers {
			// omit pseudo header names for HTTP/1, e.g. :authority, :method, :path, :scheme
			if strings.HasPrefix(key, ":") {
				continue
			}
			r.req.Header.Add(key, value)

			// prepare content length
			if strings.EqualFold(key, "Content-Length") && value != "" {
				if l, err := strconv.ParseInt(value, 10, 64); err == nil {
					r.req.ContentLength = l
				}
			}
		}
	}

	// prepare request cookies
	for cookieName, cookieValue := range r.stepRequest.Cookies {
		value, err := r.parser.Parse(cookieValue, stepVariables)
		if err != nil {
			return errors.Wrap(err, "parse cookie value failed")
		}
		r.req.AddCookie(&http.Cookie{
			Name:  cookieName,
			Value: fmt.Sprintf("%v", value),
		})
	}

	// update header
	headers := make(map[string]string)
	for key, value := range r.req.Header {
		headers[key] = value[0]
	}
	r.requestMap["headers"] = headers
	return nil
}

func (r *requestBuilder) prepareUrlParams(stepVariables map[string]interface{}) error {
	// parse step request url
	requestUrl, err := r.parser.ParseString(r.stepRequest.URL, stepVariables)
	if err != nil {
		log.Error().Err(err).Msg("parse request url failed")
		return err
	}
	rawUrl := buildURL(r.config.BaseURL, convertString(requestUrl))

	// prepare request params
	var queryParams url.Values
	if len(r.stepRequest.Params) > 0 {
		params, err := r.parser.Parse(r.stepRequest.Params, stepVariables)
		if err != nil {
			return errors.Wrap(err, "parse request params failed")
		}
		parsedParams := params.(map[string]interface{})
		r.requestMap["params"] = parsedParams
		if len(parsedParams) > 0 {
			queryParams = make(url.Values)
			for k, v := range parsedParams {
				queryParams.Add(k, fmt.Sprint(v))
			}
		}
	}
	if queryParams != nil {
		// append params to url
		paramStr := queryParams.Encode()
		if strings.IndexByte(rawUrl, '?') == -1 {
			rawUrl = rawUrl + "?" + paramStr
		} else {
			rawUrl = rawUrl + "&" + paramStr
		}
	}

	// prepare url
	u, err := url.Parse(rawUrl)
	if err != nil {
		return errors.Wrap(err, "parse url failed")
	}
	r.req.URL = u
	r.req.Host = u.Host

	return nil
}

func (r *requestBuilder) prepareBody(stepVariables map[string]interface{}) error {
	// prepare request body
	if r.stepRequest.Body == nil {
		return nil
	}

	data, err := r.parser.Parse(r.stepRequest.Body, stepVariables)
	if err != nil {
		return err
	}
	// check request body format if Content-Type specified as application/json
	if strings.HasPrefix(r.req.Header.Get("Content-Type"), "application/json") {
		switch data.(type) {
		case bool, float64, string, map[string]interface{}, []interface{}, nil:
			break
		default:
			return errors.Errorf("request body type inconsistent with Content-Type: %v",
				r.req.Header.Get("Content-Type"))
		}
	}
	r.requestMap["body"] = data
	var dataBytes []byte
	switch vv := data.(type) {
	case map[string]interface{}:
		contentType := r.req.Header.Get("Content-Type")
		if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
			// post form data
			formData := make(url.Values)
			for k, v := range vv {
				formData.Add(k, fmt.Sprint(v))
			}
			dataBytes = []byte(formData.Encode())
		} else {
			// post json
			dataBytes, err = json.Marshal(vv)
			if err != nil {
				return err
			}
			if contentType == "" {
				r.req.Header.Set("Content-Type", "application/json; charset=utf-8")
			}
		}
	case []interface{}:
		contentType := r.req.Header.Get("Content-Type")
		// post json
		dataBytes, err = json.Marshal(vv)
		if err != nil {
			return err
		}
		if contentType == "" {
			r.req.Header.Set("Content-Type", "application/json; charset=utf-8")
		}
	case string:
		dataBytes = []byte(vv)
	case []byte:
		dataBytes = vv
	case bytes.Buffer:
		dataBytes = vv.Bytes()
	default: // unexpected body type
		return errors.New("unexpected request body type")
	}

	r.req.Body = io.NopCloser(bytes.NewReader(dataBytes))
	r.req.ContentLength = int64(len(dataBytes))

	return nil
}

func runStepRequest(r *SessionRunner, step *TStep) (stepResult *StepResult, err error) {
	stepResult = &StepResult{
		Name:        step.Name,
		StepType:    stepTypeRequest,
		Success:     false,
		ContentSize: 0,
	}

	defer func() {
		// update testcase summary
		if err != nil {
			stepResult.Attachment = err.Error()
		}
	}()

	// override step variables
	stepVariables, err := r.MergeStepVariables(step.Variables)
	if err != nil {
		return
	}

	sessionData := newSessionData()
	parser := r.GetParser()
	config := r.GetConfig()

	rb := newRequestBuilder(parser, config, step.Request)
	rb.req.Method = string(step.Request.Method)

	err = rb.prepareUrlParams(stepVariables)
	if err != nil {
		return
	}

	err = rb.prepareHeaders(stepVariables)
	if err != nil {
		return
	}

	err = rb.prepareBody(stepVariables)
	if err != nil {
		return
	}

	// add request object to step variables, could be used in setup hooks
	stepVariables["hrp_step_name"] = step.Name
	stepVariables["hrp_step_request"] = rb.requestMap

	// deal with setup hooks
	for _, setupHook := range step.SetupHooks {
		_, err = parser.Parse(setupHook, stepVariables)
		if err != nil {
			return stepResult, errors.Wrap(err, "run setup hooks failed")
		}
	}

	// log & print request
	if r.LogOn() {
		if err := printRequest(rb.req); err != nil {
			return stepResult, err
		}
	}

	// do request action
	start := time.Now()
	resp, err := r.hrpRunner.client.Do(rb.req)
	stepResult.Elapsed = time.Since(start).Milliseconds()
	if err != nil {
		return stepResult, errors.Wrap(err, "do request failed")
	}
	defer resp.Body.Close()

	// decode response body in br/gzip/deflate formats
	err = decodeResponseBody(resp)
	if err != nil {
		return stepResult, errors.Wrap(err, "decode response body failed")
	}

	// log & print response
	if r.LogOn() {
		if err := printResponse(resp); err != nil {
			return stepResult, err
		}
	}

	// new response object
	respObj, err := newResponseObject(r.hrpRunner.t, parser, resp)
	if err != nil {
		err = errors.Wrap(err, "init ResponseObject error")
		return
	}

	// add response object to step variables, could be used in teardown hooks
	stepVariables["hrp_step_response"] = respObj.respObjMeta

	// deal with teardown hooks
	for _, teardownHook := range step.TeardownHooks {
		_, err = parser.Parse(teardownHook, stepVariables)
		if err != nil {
			return stepResult, errors.Wrap(err, "run teardown hooks failed")
		}
	}

	sessionData.ReqResps.Request = rb.requestMap
	sessionData.ReqResps.Response = builtin.FormatResponse(respObj.respObjMeta)

	// extract variables from response
	extractors := step.Extract
	extractMapping := respObj.Extract(extractors)
	stepResult.ExportVars = extractMapping

	// override step variables with extracted variables
	stepVariables = mergeVariables(stepVariables, extractMapping)

	// validate response
	err = respObj.Validate(step.Validators, stepVariables)
	sessionData.Validators = respObj.validationResults
	if err == nil {
		sessionData.Success = true
		stepResult.Success = true
	}
	stepResult.ContentSize = resp.ContentLength
	stepResult.Data = sessionData

	return stepResult, err
}

func printRequest(req *http.Request) error {
	reqContentType := req.Header.Get("Content-Type")
	printBody := shouldPrintBody(reqContentType)
	reqDump, err := httputil.DumpRequest(req, printBody)
	if err != nil {
		return errors.Wrap(err, "dump request failed")
	}
	fmt.Println("-------------------- request --------------------")
	reqContent := string(reqDump)
	if req.Body != nil && !printBody {
		reqContent += fmt.Sprintf("(request body omitted for Content-Type: %v)", reqContentType)
	}
	fmt.Println(reqContent)
	return nil
}

func printResponse(resp *http.Response) error {
	fmt.Println("==================== response ===================")
	respContentType := resp.Header.Get("Content-Type")
	printBody := shouldPrintBody(respContentType)
	respDump, err := httputil.DumpResponse(resp, printBody)
	if err != nil {
		return errors.Wrap(err, "dump response failed")
	}
	respContent := string(respDump)
	if !printBody {
		respContent += fmt.Sprintf("(response body omitted for Content-Type: %v)", respContentType)
	}
	fmt.Println(respContent)
	fmt.Println("--------------------------------------------------")
	return nil
}

func decodeResponseBody(resp *http.Response) (err error) {
	switch resp.Header.Get("Content-Encoding") {
	case "br":
		resp.Body = io.NopCloser(brotli.NewReader(resp.Body))
	case "gzip":
		resp.Body, err = gzip.NewReader(resp.Body)
		if err != nil {
			return err
		}
		resp.ContentLength = -1 // set to unknown to avoid Content-Length mismatched
	case "deflate":
		resp.Body, err = zlib.NewReader(resp.Body)
		if err != nil {
			return err
		}
		resp.ContentLength = -1 // set to unknown to avoid Content-Length mismatched
	}
	return nil
}

// shouldPrintBody return true if the Content-Type is printable
// including text/*, application/json, application/xml, application/www-form-urlencoded
func shouldPrintBody(contentType string) bool {
	if strings.HasPrefix(contentType, "text/") {
		return true
	}
	if strings.HasPrefix(contentType, "application/json") {
		return true
	}
	if strings.HasPrefix(contentType, "application/xml") {
		return true
	}
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		return true
	}
	return false
}

// NewStep returns a new constructed teststep with specified step name.
func NewStep(name string) *StepRequest {
	return &StepRequest{
		step: &TStep{
			Name:      name,
			Variables: make(map[string]interface{}),
		},
	}
}

type StepRequest struct {
	step *TStep
}

// WithVariables sets variables for current teststep.
func (s *StepRequest) WithVariables(variables map[string]interface{}) *StepRequest {
	s.step.Variables = variables
	return s
}

// SetupHook adds a setup hook for current teststep.
func (s *StepRequest) SetupHook(hook string) *StepRequest {
	s.step.SetupHooks = append(s.step.SetupHooks, hook)
	return s
}

// GET makes a HTTP GET request.
func (s *StepRequest) GET(url string) *StepRequestWithOptionalArgs {
	s.step.Request = &Request{
		Method: httpGET,
		URL:    url,
	}
	return &StepRequestWithOptionalArgs{
		step: s.step,
	}
}

// HEAD makes a HTTP HEAD request.
func (s *StepRequest) HEAD(url string) *StepRequestWithOptionalArgs {
	s.step.Request = &Request{
		Method: httpHEAD,
		URL:    url,
	}
	return &StepRequestWithOptionalArgs{
		step: s.step,
	}
}

// POST makes a HTTP POST request.
func (s *StepRequest) POST(url string) *StepRequestWithOptionalArgs {
	s.step.Request = &Request{
		Method: httpPOST,
		URL:    url,
	}
	return &StepRequestWithOptionalArgs{
		step: s.step,
	}
}

// PUT makes a HTTP PUT request.
func (s *StepRequest) PUT(url string) *StepRequestWithOptionalArgs {
	s.step.Request = &Request{
		Method: httpPUT,
		URL:    url,
	}
	return &StepRequestWithOptionalArgs{
		step: s.step,
	}
}

// DELETE makes a HTTP DELETE request.
func (s *StepRequest) DELETE(url string) *StepRequestWithOptionalArgs {
	s.step.Request = &Request{
		Method: httpDELETE,
		URL:    url,
	}
	return &StepRequestWithOptionalArgs{
		step: s.step,
	}
}

// OPTIONS makes a HTTP OPTIONS request.
func (s *StepRequest) OPTIONS(url string) *StepRequestWithOptionalArgs {
	s.step.Request = &Request{
		Method: httpOPTIONS,
		URL:    url,
	}
	return &StepRequestWithOptionalArgs{
		step: s.step,
	}
}

// PATCH makes a HTTP PATCH request.
func (s *StepRequest) PATCH(url string) *StepRequestWithOptionalArgs {
	s.step.Request = &Request{
		Method: httpPATCH,
		URL:    url,
	}
	return &StepRequestWithOptionalArgs{
		step: s.step,
	}
}

// CallRefCase calls a referenced testcase.
func (s *StepRequest) CallRefCase(tc ITestCase) *StepTestCaseWithOptionalArgs {
	var err error
	s.step.TestCase, err = tc.ToTestCase()
	if err != nil {
		log.Error().Err(err).Msg("failed to load testcase")
		os.Exit(1)
	}
	return &StepTestCaseWithOptionalArgs{
		step: s.step,
	}
}

// CallRefAPI calls a referenced api.
func (s *StepRequest) CallRefAPI(api IAPI) *StepAPIWithOptionalArgs {
	var err error
	s.step.API, err = api.ToAPI()
	if err != nil {
		log.Error().Err(err).Msg("failed to load api")
		os.Exit(1)
	}
	return &StepAPIWithOptionalArgs{
		step: s.step,
	}
}

// StartTransaction starts a transaction.
func (s *StepRequest) StartTransaction(name string) *StepTransaction {
	s.step.Transaction = &Transaction{
		Name: name,
		Type: transactionStart,
	}
	return &StepTransaction{
		step: s.step,
	}
}

// EndTransaction ends a transaction.
func (s *StepRequest) EndTransaction(name string) *StepTransaction {
	s.step.Transaction = &Transaction{
		Name: name,
		Type: transactionEnd,
	}
	return &StepTransaction{
		step: s.step,
	}
}

// SetThinkTime sets think time.
func (s *StepRequest) SetThinkTime(time float64) *StepThinkTime {
	s.step.ThinkTime = &ThinkTime{
		Time: time,
	}
	return &StepThinkTime{
		step: s.step,
	}
}

// StepRequestWithOptionalArgs implements IStep interface.
type StepRequestWithOptionalArgs struct {
	step *TStep
}

// SetVerify sets whether to verify SSL for current HTTP request.
func (s *StepRequestWithOptionalArgs) SetVerify(verify bool) *StepRequestWithOptionalArgs {
	s.step.Request.Verify = verify
	return s
}

// SetTimeout sets timeout for current HTTP request.
func (s *StepRequestWithOptionalArgs) SetTimeout(timeout float32) *StepRequestWithOptionalArgs {
	s.step.Request.Timeout = timeout
	return s
}

// SetProxies sets proxies for current HTTP request.
func (s *StepRequestWithOptionalArgs) SetProxies(proxies map[string]string) *StepRequestWithOptionalArgs {
	// TODO
	return s
}

// SetAllowRedirects sets whether to allow redirects for current HTTP request.
func (s *StepRequestWithOptionalArgs) SetAllowRedirects(allowRedirects bool) *StepRequestWithOptionalArgs {
	s.step.Request.AllowRedirects = allowRedirects
	return s
}

// SetAuth sets auth for current HTTP request.
func (s *StepRequestWithOptionalArgs) SetAuth(auth map[string]string) *StepRequestWithOptionalArgs {
	// TODO
	return s
}

// WithParams sets HTTP request params for current step.
func (s *StepRequestWithOptionalArgs) WithParams(params map[string]interface{}) *StepRequestWithOptionalArgs {
	s.step.Request.Params = params
	return s
}

// WithHeaders sets HTTP request headers for current step.
func (s *StepRequestWithOptionalArgs) WithHeaders(headers map[string]string) *StepRequestWithOptionalArgs {
	s.step.Request.Headers = headers
	return s
}

// WithCookies sets HTTP request cookies for current step.
func (s *StepRequestWithOptionalArgs) WithCookies(cookies map[string]string) *StepRequestWithOptionalArgs {
	s.step.Request.Cookies = cookies
	return s
}

// WithBody sets HTTP request body for current step.
func (s *StepRequestWithOptionalArgs) WithBody(body interface{}) *StepRequestWithOptionalArgs {
	s.step.Request.Body = body
	return s
}

// TeardownHook adds a teardown hook for current teststep.
func (s *StepRequestWithOptionalArgs) TeardownHook(hook string) *StepRequestWithOptionalArgs {
	s.step.TeardownHooks = append(s.step.TeardownHooks, hook)
	return s
}

// Validate switches to step validation.
func (s *StepRequestWithOptionalArgs) Validate() *StepRequestValidation {
	return &StepRequestValidation{
		step: s.step,
	}
}

// Extract switches to step extraction.
func (s *StepRequestWithOptionalArgs) Extract() *StepRequestExtraction {
	s.step.Extract = make(map[string]string)
	return &StepRequestExtraction{
		step: s.step,
	}
}

func (s *StepRequestWithOptionalArgs) Name() string {
	if s.step.Name != "" {
		return s.step.Name
	}
	return fmt.Sprintf("%v %s", s.step.Request.Method, s.step.Request.URL)
}

func (s *StepRequestWithOptionalArgs) Type() StepType {
	return StepType(fmt.Sprintf("request-%v", s.step.Request.Method))
}

func (s *StepRequestWithOptionalArgs) Struct() *TStep {
	return s.step
}

func (s *StepRequestWithOptionalArgs) Run(r *SessionRunner) (*StepResult, error) {
	return runStepRequest(r, s.step)
}

// StepRequestExtraction implements IStep interface.
type StepRequestExtraction struct {
	step *TStep
}

// WithJmesPath sets the JMESPath expression to extract from the response.
func (s *StepRequestExtraction) WithJmesPath(jmesPath string, varName string) *StepRequestExtraction {
	s.step.Extract[varName] = jmesPath
	return s
}

// Validate switches to step validation.
func (s *StepRequestExtraction) Validate() *StepRequestValidation {
	return &StepRequestValidation{
		step: s.step,
	}
}

func (s *StepRequestExtraction) Name() string {
	return s.step.Name
}

func (s *StepRequestExtraction) Type() StepType {
	return StepType(fmt.Sprintf("request-%v", s.step.Request.Method))
}

func (s *StepRequestExtraction) Struct() *TStep {
	return s.step
}

func (s *StepRequestExtraction) Run(r *SessionRunner) (*StepResult, error) {
	return runStepRequest(r, s.step)
}

// StepRequestValidation implements IStep interface.
type StepRequestValidation struct {
	step *TStep
}

func (s *StepRequestValidation) Name() string {
	if s.step.Name != "" {
		return s.step.Name
	}
	return fmt.Sprintf("%s %s", s.step.Request.Method, s.step.Request.URL)
}

func (s *StepRequestValidation) Type() StepType {
	return StepType(fmt.Sprintf("request-%v", s.step.Request.Method))
}

func (s *StepRequestValidation) Struct() *TStep {
	return s.step
}

func (s *StepRequestValidation) Run(r *SessionRunner) (*StepResult, error) {
	return runStepRequest(r, s.step)
}

func (s *StepRequestValidation) AssertEqual(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "equals",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertGreater(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "greater_than",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertLess(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "less_than",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertGreaterOrEqual(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "greater_or_equals",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertLessOrEqual(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "less_or_equals",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertNotEqual(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "not_equal",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertContains(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "contains",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertTypeMatch(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "type_match",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertRegexp(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "regex_match",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertStartsWith(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "startswith",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertEndsWith(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "endswith",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertLengthEqual(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "length_equals",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertContainedBy(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "contained_by",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertLengthLessThan(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "length_less_than",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertStringEqual(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "string_equals",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertLengthLessOrEquals(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "length_less_or_equals",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertLengthGreaterThan(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "length_greater_than",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

func (s *StepRequestValidation) AssertLengthGreaterOrEquals(jmesPath string, expected interface{}, msg string) *StepRequestValidation {
	v := Validator{
		Check:   jmesPath,
		Assert:  "length_greater_or_equals",
		Expect:  expected,
		Message: msg,
	}
	s.step.Validators = append(s.step.Validators, v)
	return s
}

// Validator represents validator for one HTTP response.
type Validator struct {
	Check   string      `json:"check" yaml:"check"` // get value with jmespath
	Assert  string      `json:"assert" yaml:"assert"`
	Expect  interface{} `json:"expect" yaml:"expect"`
	Message string      `json:"msg,omitempty" yaml:"msg,omitempty"` // optional
}
