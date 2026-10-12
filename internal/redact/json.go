package redact

import (
	"bytes"
	"encoding/json"
	"errors"
)

// A JSON object as decodeJSON reads it: every member in order, a repeated
// key included. Decoding into a map would keep only a repeated key's last
// value, and a secret in an earlier one would never be scanned.
type object []member

type member struct {
	key string
	val any
}

// get returns the value of the first member named key.
func (o object) get(key string) any {
	for _, m := range o {
		if m.key == key {
			return m.val
		}
	}
	return nil
}

func (o object) has(key string) bool {
	for _, m := range o {
		if m.key == key {
			return true
		}
	}
	return false
}

// decodeJSON reads one JSON value: an object, a []any, a string, a
// json.Number, a bool or nil. Anything but whitespace after the value is an
// error.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data[dec.InputOffset():])) > 0 {
		return nil, errors.New("data after the value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := t.(json.Delim)
	if !ok {
		return t, nil
	}
	switch d {
	case '{':
		o := object{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, ok := kt.(string)
			if !ok {
				return nil, errors.New("object key isn't a string")
			}
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			o = append(o, member{k, v})
		}
		_, err := dec.Token() // }
		return o, err
	case '[':
		a := []any{}
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := dec.Token() // ]
		return a, err
	}
	return nil, errors.New("unexpected " + d.String())
}

// encodeJSON writes a value decodeJSON read, members in their order, without
// escaping HTML characters, as Claude Code writes its transcripts.
func encodeJSON(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case object:
		b.WriteByte('{')
		for i, m := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := encodeString(b, m.key); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := encodeJSON(b, m.val); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := encodeJSON(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case string:
		return encodeString(b, x)
	case json.Number:
		b.WriteString(string(x))
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case nil:
		b.WriteString("null")
	default:
		return errors.New("unexpected JSON value")
	}
	return nil
}

func encodeString(b *bytes.Buffer, s string) error {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	b.Write(bytes.TrimSuffix(tmp.Bytes(), []byte("\n")))
	return nil
}
