// Copyright 2017 Santhosh Kumar Tekuri. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jsonschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/formats"
)

// A Schema represents compiled version of json-schema.
type Schema struct {
	url *string
	ptr *string

	// type agnostic validations
	ref       *Schema
	types     []string
	enum      []interface{}
	enumError string // error message for enum fail
	not       *Schema
	allOf     []*Schema
	anyOf     []*Schema
	oneOf     []*Schema

	// object validations
	minProperties        int // -1 if not specified
	maxProperties        int // -1 if not specified
	required             []string
	properties           map[string]*Schema
	regexProperties      bool // property names must be valid regex
	patternProperties    map[*regexp.Regexp]*Schema
	additionalProperties interface{}            // nil or false or *Schema
	dependencies         map[string]interface{} // value is *Schema or []string

	// array validations
	minItems        int // -1 if not specified
	maxItems        int // -1 if not specified
	uniqueItems     bool
	items           interface{} // nil or *Schema or []*Schema
	additionalItems interface{} // nil or bool or *Schema

	// string validations
	minLength  int // -1 if not specified
	maxLength  int // -1 if not specified
	pattern    *regexp.Regexp
	format     formats.Format
	formatName string

	// number validators
	minimum          *big.Float
	exclusiveMinimum bool
	maximum          *big.Float
	exclusiveMaximum bool
	multipleOf       *big.Float
}

// Compile parses json-schema at given url returns, if successful,
// a Schema object that can be used to match against json.
//
// The json-schema is validated with draft4 specification.
// Returned error can be *SchemaError
func Compile(url string) (*Schema, error) {
	return NewCompiler().Compile(url)
}

// MustCompile is like Compile but panics if the url cannot be compiled to *Schema.
// It simplifies safe initialization of global variables holding compiled Schemas.
func MustCompile(url string) *Schema {
	s, err := Compile(url)
	if err != nil {
		panic(fmt.Sprintf("jsonschema: Compile(%q): %s", url, err))
	}
	return s
}

// Validate validates the given json data, against the json-schema,
//
// Returned error can be *ValidationError.
func (s *Schema) Validate(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var doc interface{}
	if err := decoder.Decode(&doc); err != nil {
		return err
	}
	if t, _ := decoder.Token(); t != nil {
		return fmt.Errorf("invalid character %v after top-level value", t)
	}
	if err := s.validate(doc); err != nil {
		finishContext(err, s)
		return err
	}
	return nil
}

func (s *Schema) validate(v interface{}) error {
	if s.ref != nil {
		if err := s.ref.validate(v); err != nil {
			finishContext(err, s.ref)
			return validationError("$ref", "$ref failed").add(err)
		}

		// All other properties in a "$ref" object MUST be ignored
		return nil
	}

	if len(s.types) > 0 {
		vType := jsonType(v)
		matched := false
		for _, t := range s.types {
			if vType == t {
				matched = true
				break
			} else if t == "integer" && vType == "number" {
				if _, ok := new(big.Int).SetString(string(v.(json.Number)), 10); ok {
					matched = true
					break
				}
			}
		}
		if !matched {
			return validationError("type", "expected %s, but got %s", strings.Join(s.types, " or "), vType)
		}
	}

	if len(s.enum) > 0 {
		matched := false
		for _, item := range s.enum {
			if equals(v, item) {
				matched = true
				break
			}
		}
		if !matched {
			return validationError("enum", s.enumError)
		}
	}

	if s.not != nil && s.not.validate(v) == nil {
		return validationError("not", "not failed")
	}

	for i, sch := range s.allOf {
		if err := sch.validate(v); err != nil {
			return validationError("allOf/"+strconv.Itoa(i), "allOf failed").add(err)
		}
	}

	if len(s.anyOf) > 0 {
		matched := false
		var causes []error
		for i, sch := range s.anyOf {
			if err := sch.validate(v); err == nil {
				matched = true
				break
			} else {
				causes = append(causes, addContext("", strconv.Itoa(i), err))
			}
		}
		if !matched {
			return validationError("anyOf", "anyOf failed").add(causes...)
		}
	}

	if len(s.oneOf) > 0 {
		matched := -1
		var causes []error
		for i, sch := range s.oneOf {
			if err := sch.validate(v); err == nil {
				if matched == -1 {
					matched = i
				} else {
					return validationError("oneOf", "valid against schemas at indexes %d and %d", matched, i)
				}
			} else {
				causes = append(causes, addContext("", strconv.Itoa(i), err))
			}
		}
		if matched == -1 {
			return validationError("oneOf", "oneOf failed").add(causes...)
		}
	}

	switch v := v.(type) {
	case map[string]interface{}:
		if s.minProperties != -1 && len(v) < s.minProperties {
			return validationError("minProperties", "minimum %d properties allowed, but found %d properties", s.minProperties, len(v))
		}
		if s.maxProperties != -1 && len(v) > s.maxProperties {
			return validationError("maxProperties", "maximum %d properties allowed, but found %d properties", s.maxProperties, len(v))
		}
		if len(s.required) > 0 {
			var missing []string
			for _, pname := range s.required {
				if _, ok := v[pname]; !ok {
					missing = append(missing, pname)
				}
			}
			if len(missing) > 0 {
				return validationError("required", "missing properties: %s", strings.Join(missing, ", "))
			}
		}

		var additionalProps map[string]struct{}
		if s.additionalProperties != nil {
			additionalProps = make(map[string]struct{}, len(v))
			for pname := range v {
				additionalProps[pname] = struct{}{}
			}
		}

		if len(s.properties) > 0 {
			for pname, pschema := range s.properties {
				if pvalue, ok := v[pname]; ok {
					delete(additionalProps, pname)
					if err := pschema.validate(pvalue); err != nil {
						return addContext(escape(pname), "properties/"+escape(pname), err) // todo pname escaping in sptr
					}
				}
			}
		}
		if s.regexProperties {
			for pname := range v {
				if !formats.IsRegex(pname) {
					return validationError("", "patternProperty %q is not valid regex", pname)
				}
			}
		}
		for pattern, pschema := range s.patternProperties {
			for pname, pvalue := range v {
				if pattern.MatchString(pname) {
					delete(additionalProps, pname)
					if err := pschema.validate(pvalue); err != nil {
						return addContext(escape(pname), "patternProperties/"+escape(pattern.String()), err) // todo pattern escaping in sptr
					}
				}
			}
		}
		if s.additionalProperties != nil {
			if _, ok := s.additionalProperties.(bool); ok {
				if len(additionalProps) != 0 {
					pnames := make([]string, 0, len(additionalProps))
					for pname := range additionalProps {
						pnames = append(pnames, pname)
					}
					return validationError("additionalProperties", "additionalProperties %s not allowed", strings.Join(pnames, ", "))
				}
			} else {
				schema := s.additionalProperties.(*Schema)
				for pname := range additionalProps {
					if pvalue, ok := v[pname]; ok {
						if err := schema.validate(pvalue); err != nil {
							return addContext(escape(pname), "additionalProperties", err)
						}
					}
				}
			}
		}
		for dname, dvalue := range s.dependencies {
			if _, ok := v[dname]; ok {
				switch dvalue := dvalue.(type) {
				case *Schema:
					if err := dvalue.validate(v); err != nil {
						return addContext("", "dependencies/"+escape(dname), err)
					}
				case []string:
					for i, pname := range dvalue {
						if _, ok := v[pname]; !ok {
							return validationError("dependencies/"+escape(dname)+"/"+strconv.Itoa(i), "property %s is required, if %s property exists", pname, dname)
						}
					}
				}
			}
		}

	case []interface{}:
		if s.minItems != -1 && len(v) < s.minItems {
			return validationError("minItems", "minimum %d items allowed, but found %d items", s.minItems, len(v))
		}
		if s.maxItems != -1 && len(v) > s.maxItems {
			return validationError("maxItems", "maximum %d items allowed, but found %d items", s.maxItems, len(v))
		}
		if s.uniqueItems {
			for i := 1; i < len(v); i++ {
				for j := 0; j < i; j++ {
					if equals(v[i], v[j]) {
						return validationError("uniqueItems", "items at index %d and %d are equal", j, i)
					}
				}
			}
		}
		switch items := s.items.(type) {
		case *Schema:
			for i, item := range v {
				if err := items.validate(item); err != nil {
					return addContext(strconv.Itoa(i), "items", err)
				}
			}
		case []*Schema:
			if additionalItems, ok := s.additionalItems.(bool); ok {
				if !additionalItems && len(v) > len(items) {
					return validationError("additionalItems", "only %d items are allowed, but found %d items", len(items), len(v))
				}
			}
			for i, item := range v {
				if i < len(items) {
					if err := items[i].validate(item); err != nil {
						return addContext(strconv.Itoa(i), "items/"+strconv.Itoa(i), err)
					}
				} else if sch, ok := s.additionalItems.(*Schema); ok {
					if err := sch.validate(item); err != nil {
						return addContext(strconv.Itoa(i), "additionalItems", err)
					}
				} else {
					break
				}
			}
		}

	case string:
		if s.minLength != -1 || s.maxLength != -1 {
			length := utf8.RuneCount([]byte(v))
			if s.minLength != -1 && length < s.minLength {
				return validationError("minLength", "length must be >= %d, but got %d", s.minLength, length)
			}
			if s.maxLength != -1 && length > s.maxLength {
				return validationError("maxLength", "length must be <= %d, but got %d", s.maxLength, length)
			}
		}
		if s.pattern != nil && !s.pattern.MatchString(v) {
			return validationError("pattern", "does not match pattern %s", s.pattern)
		}
		if s.format != nil && !s.format(v) {
			return validationError("format", "%q is not valid %s", v, s.formatName)
		}

	case json.Number:
		if s.minimum != nil {
			v, _ := new(big.Float).SetString(string(v))
			cmp := v.Cmp(s.minimum)
			if s.exclusiveMinimum {
				if cmp <= 0 {
					return validationError("minimum", "must be > %v but found %v", s.minimum, v)
				}
			} else {
				if cmp < 0 {
					return validationError("minimum", "must be >= %v but found %v", s.minimum, v)
				}
			}
		}
		if s.maximum != nil {
			v, _ := new(big.Float).SetString(string(v))
			cmp := v.Cmp(s.maximum)
			if s.exclusiveMaximum {
				if cmp >= 0 {
					return validationError("maximum", "must be < %v but found %v", s.maximum, v)
				}
			} else {
				if cmp > 0 {
					return validationError("maximum", "must be <= %v but found %v", s.maximum, v)
				}
			}
		}
		if s.multipleOf != nil {
			v, _ := new(big.Float).SetString(string(v))
			if q := new(big.Float).Quo(v, s.multipleOf); !q.IsInt() {
				return validationError("multipleOf", "%v not multipleOf %v", v, s.multipleOf)
			}
		}
	}

	return nil
}

func jsonType(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		panic(fmt.Sprintf("unexpected jsonType: %T", v))
	}
}

func equals(v1, v2 interface{}) bool {
	v1Type, v2Type := jsonType(v1), jsonType(v2)
	if v1Type != v2Type {
		return false
	}
	switch v1Type {
	case "array":
		arr1, arr2 := v1.([]interface{}), v2.([]interface{})
		if len(arr1) != len(arr2) {
			return false
		}
		for i := range arr1 {
			if !equals(arr1[i], arr2[i]) {
				return false
			}
		}
		return true
	case "object":
		obj1, obj2 := v1.(map[string]interface{}), v2.(map[string]interface{})
		if len(obj1) != len(obj2) {
			return false
		}
		for k := range obj1 {
			if !equals(obj1[k], obj2[k]) {
				return false
			}
		}
		return true
	case "number":
		num1, _ := new(big.Float).SetString(string(v1.(json.Number)))
		num2, _ := new(big.Float).SetString(string(v2.(json.Number)))
		return num1.Cmp(num2) == 0
	default:
		return v1 == v2
	}
}

func escape(token string) string {
	token = strings.Replace(token, "~", "~0", -1)
	token = strings.Replace(token, "/", "~1", -1)
	return url.PathEscape(token)
}
