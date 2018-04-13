package logger

import (
	"encoding/json"
	"io"
	"os"
)

const (
	// JSONFieldLabel is a common json field.
	JSONFieldLabel = "label"
	// JSONFieldFlag is a common json field.
	JSONFieldFlag = "flag"
	// JSONFieldTimestamp is a common json field.
	JSONFieldTimestamp = "ts"
	// JSONFieldMessage is a common json field.
	JSONFieldMessage = "message"
	// JSONFieldElapsed is a common json field.
	JSONFieldElapsed = "elapsed"
	// JSONFieldErr is a common json field.
	JSONFieldErr = "err"
	// JSONFieldEventHeadings is a common json field.
	JSONFieldEventHeadings = "event-headings"

	// DefaultJSONWriterPretty is a default.
	DefaultJSONWriterPretty = false
)

// Asserts text writer is a writer.
var (
	_ Writer = &TextWriter{}
)

// JSONObj is a type alias for map[string]Any
type JSONObj = Values

// JSONWritable is a type with a custom formater for json writing.
type JSONWritable interface {
	WriteJSON() JSONObj
}

// NewJSONWriter returns a json writer with defaults.
func NewJSONWriter(output io.Writer) *JSONWriter {
	return &JSONWriter{
		output: NewInterlockedWriter(output),
		pretty: DefaultJSONWriterPretty,
	}
}

// NewJSONWriterStdout returns a new text writer to stdout/stderr.
func NewJSONWriterStdout() *JSONWriter {
	return NewJSONWriter(os.Stdout).WithErrorOutput(os.Stderr)
}

// NewJSONWriterFromEnv returns a new json writer from the environment.
func NewJSONWriterFromEnv() *JSONWriter {
	return NewJSONWriterFromConfig(NewJSONWriterConfigFromEnv())
}

// NewJSONWriterFromConfig returns a new json writer from a config.
func NewJSONWriterFromConfig(cfg *JSONWriterConfig) *JSONWriter {
	return &JSONWriter{
		output:      NewInterlockedWriter(os.Stdout),
		errorOutput: NewInterlockedWriter(os.Stderr),
		pretty:      cfg.GetPretty(),
	}
}

// JSONWriter is a json output format.
type JSONWriter struct {
	output      io.Writer
	errorOutput io.Writer
	label       string
	pretty      bool
}

// OutputFormat returns the output format.
func (jw *JSONWriter) OutputFormat() OutputFormat {
	return OutputFormatJSON
}

// WithOutput sets the primary output.
func (jw *JSONWriter) WithOutput(output io.Writer) *JSONWriter {
	jw.output = NewInterlockedWriter(output)
	return jw
}

// WithErrorOutput sets the error output.
func (jw *JSONWriter) WithErrorOutput(errorOutput io.Writer) *JSONWriter {
	jw.errorOutput = NewInterlockedWriter(errorOutput)
	return jw
}

// Label returns a descriptive label for the writer.
func (jw *JSONWriter) Label() string {
	return jw.label
}

// WithLabel sets the writer label.
func (jw *JSONWriter) WithLabel(label string) Writer {
	jw.label = label
	return jw
}

// Output returns an io.Writer for the ouptut stream.
func (jw *JSONWriter) Output() io.Writer {
	return jw.output
}

// ErrorOutput returns an io.Writer for the error stream.
func (jw *JSONWriter) ErrorOutput() io.Writer {
	if jw.errorOutput != nil {
		return jw.errorOutput
	}
	return jw.output
}

// Write writes to stdout.
func (jw *JSONWriter) Write(e Event) error {
	return jw.write(jw.output, e)
}

// WriteError writes to stderr (or stdout if .errorOutput is unset).
func (jw *JSONWriter) WriteError(e Event) error {
	return jw.write(jw.ErrorOutput(), e)
}

func (jw *JSONWriter) write(output io.Writer, e Event) error {
	encoder := json.NewEncoder(output)
	if jw.pretty {
		encoder.SetIndent("", "\t")
	}

	if typed, isTyped := e.(JSONWritable); isTyped {
		fields := typed.WriteJSON()
		if len(jw.label) > 0 {
			fields[JSONFieldLabel] = jw.label
		}
		if typed, isTyped := e.(EventHeadings); isTyped && len(typed.Headings()) > 0 {
			fields[JSONFieldEventHeadings] = typed.Headings()
		}
		fields[JSONFieldFlag] = e.Flag()
		fields[JSONFieldTimestamp] = e.Timestamp()
		return encoder.Encode(fields)
	}

	return encoder.Encode(e)
}
