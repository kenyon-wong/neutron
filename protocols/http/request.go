package http

import (
	"errors"
	"fmt"
	"github.com/chainreactors/neutron/common"
	"github.com/chainreactors/neutron/operators"
	"github.com/chainreactors/neutron/protocols"
	"github.com/chainreactors/parsers"
	"github.com/chainreactors/utils/iutils"
	"io"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var errStopExecution = errors.New("stop execution due to unresolved variables")
var _ protocols.Request = &Request{}

type Request struct {
	// operators for the current request go here.
	operators.Operators `json:",inline" yaml:",inline"`
	// Path contains the path/s for the request
	Path []string `json:"path" yaml:"path"`
	// Raw contains raw requests
	Raw []string `json:"raw" yaml:"raw"`
	ID  string   `json:"id" yaml:"id"`
	// Name is the name of the request
	Name string `json:"name" yaml:"name"`
	// AttackType is the attack type
	// Sniper, PitchFork and ClusterBomb. Default is Sniper
	AttackType string `json:"attack" yaml:"attack"`
	// Method is the request method, whether GET, POST, PUT, etc
	Method string `json:"method" yaml:"method"`
	// Body is an optional parameter which contains the request body for POST methods, etc
	Body string `json:"body" yaml:"body"`
	// Path contains the path/s for the request variables
	Payloads map[string]interface{} `json:"payloads" yaml:"payloads"`
	// Headers contains headers to send with the request
	Headers map[string]string `json:"headers" yaml:"headers"`
	// MaxRedirects is the maximum number of redirects that should be followed.
	MaxRedirects int `json:"max-redirects" yaml:"max-redirects"`
	// PipelineConcurrentConnections is number of connections in pipelining
	Threads int `json:"threads" yaml:"threads"`

	// MaxSize is the maximum size of http response body to read in bytes.
	MaxSize int `json:"max-size" yaml:"max-size"`

	// CookieReuse is an optional setting that makes cookies shared within requests
	CookieReuse bool `json:"cookie-reuse" yaml:"cookie-reuse"`
	// Redirects specifies whether redirects should be followed.
	Redirects bool `json:"redirects" yaml:"redirects"`
	// Pipeline defines if the attack should be performed with HTTP 1.1 Pipelining (race conditions/billions requests)
	// All requests must be indempotent (GET/POST)
	Unsafe bool `json:"unsafe" yaml:"unsafe"`
	// ReqCondition automatically assigns numbers to requests and preserves
	// their history for being matched at the end.
	// Currently only works with sequential http requests.
	ReqCondition bool `json:"req-condition" yaml:"req-condition"`
	//   StopAtFirstMatch stops the execution of the requests and template as soon as a match is found.
	StopAtFirstMatch  bool                 `json:"stop-at-first-match" yaml:"stop-at-first-match"`
	generator         *protocols.Generator // optional, only enabled when using payloads
	httpClient        *http.Client
	httpresp          *http.Response
	CompiledOperators *operators.Operators
	attackType        protocols.Type
	totalRequests     int

	options *protocols.ExecuterOptions
	//Result            *protocols.Result
}

// Type returns the type of the protocol request
func (r *Request) Type() protocols.ProtocolType {
	return protocols.FileProtocol
}

// Match matches a generic data response again a given matcher
func (r *Request) Match(data map[string]interface{}, matcher *operators.Matcher) bool {
	item, ok := r.getMatchPart(matcher.Part, data)
	if !ok {
		return false
	}

	switch matcher.GetType() {
	case operators.StatusMatcher:
		statusCode, ok := data["status_code"]
		if !ok {
			return false
		}
		status, ok := statusCode.(int)
		if !ok {
			return false
		}
		return matcher.Result(matcher.MatchStatusCode(status))
	case operators.SizeMatcher:
		return matcher.Result(matcher.MatchSize(len(item)))
	case operators.WordsMatcher:
		return matcher.Result(matcher.MatchWords(item))
	case operators.RegexMatcher:
		return matcher.Result(matcher.MatchRegex(item))
	case operators.BinaryMatcher:
		return matcher.Result(matcher.MatchBinary(item))
	}
	return false
}

// Extract performs extracting operation for an extractor on model and returns true or false.
func (r *Request) Extract(data map[string]interface{}, extractor *operators.Extractor) map[string]struct{} {
	item, ok := r.getMatchPart(extractor.Part, data)
	if !ok {
		return nil
	}
	switch extractor.GetType() {
	case operators.RegexExtractor:
		return extractor.ExtractRegex(item)
	case operators.KValExtractor:
		return extractor.ExtractKval(data)
	}
	return nil
}

// getMatchPart returns the match part honoring "all" matchers + others.
func (r *Request) getMatchPart(part string, data protocols.InternalEvent) (string, bool) {
	if part == "" {
		part = "body"
	}
	if part == "header" {
		part = "all_headers"
	}
	var itemStr string

	if part == "all" {
		builder := &strings.Builder{}
		builder.WriteString(iutils.ToString(data["body"]))
		builder.WriteString(iutils.ToString(data["all_headers"]))
		itemStr = builder.String()
	} else {
		item, ok := data[part]
		if !ok {
			return "", false
		}
		itemStr = iutils.ToString(item)
	}
	return itemStr, true
}

func (r *Request) GetCompiledOperators() []*operators.Operators {
	return []*operators.Operators{r.CompiledOperators}
}

//var (
//	urlWithPortRegex = regexp.MustCompile(`{{BaseURL}}:(\d+)`)
//)
// MakeResultEvent creates a result event from internal wrapped event
func (r *Request) MakeResultEvent(wrapped *protocols.InternalWrappedEvent) []*protocols.ResultEvent {
	if len(wrapped.OperatorsResult.DynamicValues) > 0 && !wrapped.OperatorsResult.Matched {
		return nil
	}

	results := make([]*protocols.ResultEvent, 0, len(wrapped.OperatorsResult.Matches)+1)

	// If we have multiple matchers with names, write each of them separately.
	if len(wrapped.OperatorsResult.Matches) > 0 {
		for k := range wrapped.OperatorsResult.Matches {
			data := r.MakeResultEventItem(wrapped)
			data.MatcherName = k
			results = append(results, data)
		}
	} else if len(wrapped.OperatorsResult.Extracts) > 0 {
		for k, v := range wrapped.OperatorsResult.Extracts {
			data := r.MakeResultEventItem(wrapped)
			data.ExtractedResults = v
			data.ExtractorName = k
			results = append(results, data)
		}
	} else {
		data := r.MakeResultEventItem(wrapped)
		results = append(results, data)
	}
	return results
}

func (r *Request) MakeResultEventItem(wrapped *protocols.InternalWrappedEvent) *protocols.ResultEvent {
	data := &protocols.ResultEvent{
		TemplateID: iutils.ToString(wrapped.InternalEvent["template-id"]),
		//Info:             wrapped.InternalEvent["template-info"].(map[string]interface{}),
		Type:             "http",
		Host:             iutils.ToString(wrapped.InternalEvent["host"]),
		Matched:          iutils.ToString(wrapped.InternalEvent["matched"]),
		Metadata:         wrapped.OperatorsResult.PayloadValues,
		ExtractedResults: wrapped.OperatorsResult.OutputExtracts,
		Timestamp:        time.Now(),
		IP:               iutils.ToString(wrapped.InternalEvent["ip"]),
	}
	return data
}

// requests returns the total number of requests the YAML rule will perform
func (r *Request) Requests() int {
	if r.generator != nil {
		payloadRequests := r.generator.NewIterator().Total() * len(r.Raw)
		return payloadRequests
	}
	if len(r.Raw) > 0 {
		requests := len(r.Raw)
		return requests
	}
	return len(r.Path)
}

func (r *Request) Compile(options *protocols.ExecuterOptions) error {
	r.options = options
	var err error

	connectionConfiguration := &Configuration{
		//Threads:         r.Threads,
		Timeout:         DefaultOption.Timeout,
		MaxRedirects:    r.MaxRedirects,
		FollowRedirects: r.Redirects,
		CookieReuse:     r.CookieReuse,
	}
	r.httpClient = createClient(connectionConfiguration)

	if r.Body != "" && !strings.Contains(r.Body, "\r\n") {
		r.Body = strings.Replace(r.Body, "\n", "\r\n", -1)
	}
	if len(r.Raw) > 0 {
		for i, raw := range r.Raw {
			if !strings.Contains(raw, "\r\n") {
				r.Raw[i] = strings.Replace(raw, "\n", "\r\n", -1)
			}
		}
	}

	// 修改: 只编译一次Matcher
	if len(r.Matchers) > 0 || len(r.Extractors) > 0 {
		compiled := &r.Operators
		if compileErr := compiled.Compile(); compileErr != nil {
			return compileErr
		}
		r.CompiledOperators = compiled
	}

	if len(r.Payloads) > 0 {
		var attackType string
		if r.options.Options.AttackType != "" {
			attackType = r.options.Options.AttackType
		} else if len(r.options.Options.VarsPayload) > 0 {
			attackType = "clusterbomb"
		} else if r.AttackType != "" {
			attackType = r.AttackType
		} else {
			attackType = "sniper"
		}

		r.attackType = protocols.StringToType[attackType]
		// 允许使用命令行定义对应的参数, 会替换对应的参数, 如果参数的数量对不上可能会报错
		for k, v := range r.options.Options.VarsPayload {
			if _, ok := r.Payloads[k]; ok {
				r.Payloads[k] = v
			}
		}
		for k, payload := range r.Payloads {
			switch payload.(type) {
			case []string:
				tmp := make([]string, len(payload.([]string)))
				for i, p := range payload.([]string) {
					tmp[i], _ = parsers.DSLParserToString(iutils.ToString(p))
				}
				r.Payloads[k] = tmp
			}

		}
		r.generator, err = protocols.New(r.Payloads, r.attackType)
		if err != nil {
			return err
		}
	}
	r.totalRequests = r.Requests()
	return nil
}

func (r *Request) ExecuteWithResults(input string, dynamicValues map[string]interface{}, callback protocols.OutputEventCallback) error {
	err := r.ExecuteRequestWithResults(input, dynamicValues, callback)
	if err != nil {
		return err
	}
	return nil
}

func (r *Request) ExecuteRequestWithResults(reqURL string, dynamicValues map[string]interface{}, callback protocols.OutputEventCallback) error {
	generator := r.newGenerator()
	requestCount := 1
	var requestErr error
	var gotDynamicValues map[string][]string
	for {
		// returns two values, error and skip, which skips the execution for the request instance.
		executeFunc := func(data string, payloads, dynamicValue map[string]interface{}) (bool, error) {
			generatedHttpRequest, err := generator.Make(reqURL, data, payloads, dynamicValue)
			if err != nil {
				if err == io.EOF {
					return true, nil
				}
				return true, err
			}
			if generatedHttpRequest.request.Header.Get("User-Agent") == "" {
				generatedHttpRequest.request.Header.Set("User-Agent", ua)
			}
			var gotMatches bool
			err = r.executeRequest(generatedHttpRequest, dynamicValues, func(event *protocols.InternalWrappedEvent) {
				// Add the extracts to the dynamic values if any.
				if event.OperatorsResult != nil {
					gotMatches = event.OperatorsResult.Matched
					gotDynamicValues = common.MergeMapsMany(event.OperatorsResult.DynamicValues, dynamicValues, gotDynamicValues)
				}
				callback(event)
			})

			// If a variable is unresolved, skip all further requests
			if err == errStopExecution {
				return true, nil
			}
			if err != nil {
				requestErr = err
			}
			requestCount++
			//request.options.Progress.IncrementRequests()

			// If this was a match, and we want to stop at first match, skip all further requests.
			if r.StopAtFirstMatch && gotMatches {
				return true, nil
			}
			return false, nil
		}

		inputData, payloads, ok := generator.nextValue()
		if !ok {
			break
		}
		if len(payloads) > 0 {
			common.NeutronLog.Debugf("payloads: %s", iutils.MapToString(payloads))
		}
		var gotErr error
		var skip bool

		if len(gotDynamicValues) > 0 {
			operators.MakeDynamicValuesCallback(gotDynamicValues, true, func(data map[string]interface{}) bool {
				if skip, gotErr = executeFunc(inputData, payloads, data); skip || gotErr != nil {
					return true
				}
				return false
			})
		} else {
			skip, gotErr = executeFunc(inputData, payloads, dynamicValues)
		}
		if gotErr != nil && requestErr == nil {
			requestErr = gotErr
		}
		if skip || gotErr != nil {
			break
		}
	}
	return requestErr
}

func (r *Request) executeRequest(request *generatedRequest, dynamicValues map[string]interface{}, callback protocols.OutputEventCallback) error {
	resp, err := r.httpClient.Do(request.request)
	if err != nil {
		common.NeutronLog.Debugf("%s nuclei request failed, %s", request.request.URL, err.Error())
		return err
	}
	data := respToMap(resp, request.request)
	event := &protocols.InternalWrappedEvent{InternalEvent: dynamicValues}
	if r.CompiledOperators != nil {
		var ok bool
		event.OperatorsResult, ok = r.CompiledOperators.Execute(data, r.Match, r.Extract)
		if ok && event.OperatorsResult != nil {
			event.OperatorsResult.PayloadValues = request.meta
			event.Results = r.MakeResultEvent(event)
			callback(event)
			return nil
		}
	}
	return err
}

func respToMap(resp *http.Response, req *http.Request) map[string]interface{} {
	data := make(map[string]interface{})
	data["host"] = req.Host
	data["request"] = req
	data["response"] = resp
	data["content_length"] = resp.ContentLength
	data["status_code"] = resp.StatusCode
	bodybytes, _ := ioutil.ReadAll(resp.Body)
	data["body"] = string(bodybytes)
	data["url"] = req.URL

	for k, v := range resp.Header {
		for _, i := range v {
			data["all_headers"] = iutils.ToString(data["all_headers"]) + fmt.Sprintf("%s: %s\r\n", k, i)
		}
	}

	for _, cookie := range resp.Cookies() {
		data[strings.ToLower(cookie.Name)] = cookie.Value
	}
	for k, v := range resp.Header {
		k = strings.ToLower(strings.Replace(strings.TrimSpace(k), "-", "_", -1))
		data[k] = strings.Join(v, " ")
	}
	resp.Body.Close()
	return data
}

func (r *Request) GetID() string {
	return r.ID
}

var (
	urlWithPortRegex = regexp.MustCompile(`{{BaseURL}}:(\d+)`)
)

//generatedRequest is a single wrapped generated request for a template request
type generatedRequest struct {
	original *Request
	//rawRequest      *raw.Request
	meta map[string]interface{}
	//pipelinedClient *rawhttp.PipelineClient
	request       *http.Request
	dynamicValues map[string]interface{}
}
