package dahua

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// maxIVSDataBytes bounds an IVS data block; real ones are a few KB.
const maxIVSDataBytes = 64 << 10

// ivsBlock accumulates the JSON data block of a tripwire or intrusion
// (CrossLineDetection / CrossRegionDetection) event:
//
//	Code=CrossRegionDetection;action=Start;index=0;data={
//	   "Object" : { "ObjectType" : "Human", ... },
//	   ...
//	}
//
// The block may also arrive whole on the code line.
type ivsBlock struct {
	code    string
	channel int
	first   string // the data text on the code line, starting at "{"
	data    strings.Builder
	depth   int
	started bool
}

// startIVS recognizes the start of an IVS event worth inspecting: a starting
// (Start, or Pulse for single-shot rules) tripwire or intrusion event that
// carries data. It returns nil for anything else.
func startIVS(line string) *ivsBlock {
	var code string
	switch {
	case strings.HasPrefix(line, "Code=CrossLineDetection;"):
		code = "CrossLineDetection"
	case strings.HasPrefix(line, "Code=CrossRegionDetection;"):
		code = "CrossRegionDetection"
	default:
		return nil
	}
	head, data, ok := strings.Cut(line, ";data=")
	if !ok {
		return nil
	}
	fields := make(map[string]string)
	for _, part := range strings.Split(head, ";") {
		if k, v, ok := strings.Cut(part, "="); ok {
			fields[k] = v
		}
	}
	// Cameras differ in case here: the API docs show both "Start" and
	// "pulse".
	action := fields["action"]
	if !strings.EqualFold(action, "Start") && !strings.EqualFold(action, "Pulse") {
		return nil
	}
	channel, _ := strconv.Atoi(fields["index"])
	return &ivsBlock{code: code, channel: channel, first: data}
}

// feed adds a line of the data block and reports whether the block is
// finished: its braces balanced, or abandoned (with an error) because it grew
// too large or the multipart framing resumed before it closed.
func (b *ivsBlock) feed(line string) (bool, error) {
	if strings.HasPrefix(line, "--") {
		return true, fmt.Errorf("%s data block cut off by the next part", b.code)
	}
	b.data.WriteString(line)
	b.data.WriteByte('\n')
	if b.data.Len() > maxIVSDataBytes {
		return true, fmt.Errorf("%s data block larger than %d bytes", b.code, maxIVSDataBytes)
	}
	// Brace counting ignores braces inside JSON strings; object type names
	// and the like never contain them.
	for _, r := range line {
		switch r {
		case '{':
			b.depth++
			b.started = true
		case '}':
			b.depth--
		}
	}
	return b.started && b.depth <= 0, nil
}

// human reports whether the block's object, or any of its objects, is a
// human.
func (b *ivsBlock) human() (bool, error) {
	var data struct {
		Object  ivsObject   `json:"Object"`
		Objects []ivsObject `json:"Objects"`
	}
	if err := json.Unmarshal([]byte(b.data.String()), &data); err != nil {
		return false, fmt.Errorf("%s data block: %w", b.code, err)
	}
	if data.Object.isHuman() {
		return true, nil
	}
	for _, o := range data.Objects {
		if o.isHuman() {
			return true, nil
		}
	}
	return false, nil
}

type ivsObject struct {
	ObjectType string `json:"ObjectType"`
}

func (o ivsObject) isHuman() bool {
	return strings.EqualFold(o.ObjectType, "Human")
}
