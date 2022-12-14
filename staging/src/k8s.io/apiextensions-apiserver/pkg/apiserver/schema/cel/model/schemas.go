/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package model

import (
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"

	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
)

const (
	// the largest request that will be accepted is 3MB
	// TODO(DangerOnTheRanger): wire in MaxRequestBodyBytes from apiserver/pkg/server/options/server_run_options.go to make this configurable
	maxRequestSizeBytes = int64(3 * 1024 * 1024)
	// OpenAPI duration strings follow RFC 3339, section 5.6 - see the comment on maxDatetimeSizeJSON
	maxDurationSizeJSON = 32
	// OpenAPI datetime strings follow RFC 3339, section 5.6, and the longest possible
	// such string is 9999-12-31T23:59:59.999999999Z, which has length 30 - we add 2
	// to allow for quotation marks
	maxDatetimeSizeJSON = 32
	// Golang allows a string of 0 to be parsed as a duration, so that plus 2 to account for
	// quotation marks makes 3
	minDurationSizeJSON = 3
	// RFC 3339 dates require YYYY-MM-DD, and then we add 2 to allow for quotation marks
	dateSizeJSON = 12
	// RFC 3339 datetimes require a full date (YYYY-MM-DD) and full time (HH:MM:SS), and we add 3 for
	// quotation marks like always in addition to the capital T that separates the date and time
	minDatetimeSizeJSON = 21
	// ""
	minStringSize = 2
	// true
	minBoolSize = 4
	// 0
	minNumberSize = 1
)

// SchemaDeclType converts the structural schema to a CEL declaration, or returns nil if the
// the structural schema should not be exposed in CEL expressions.
// Set isResourceRoot to true for the root of a custom resource or embedded resource.
//
// Schemas with XPreserveUnknownFields not exposed unless they are objects. Array and "maps" schemas
// are not exposed if their items or additionalProperties schemas are not exposed. Object Properties are not exposed
// if their schema is not exposed.
//
// The CEL declaration for objects with XPreserveUnknownFields does not expose unknown fields.
func SchemaDeclType(s *schema.Structural, isResourceRoot bool) *DeclType {
	if s == nil {
		return nil
	}
	if s.XIntOrString {
		// schemas using XIntOrString are not required to have a type.

		// intOrStringType represents the x-kubernetes-int-or-string union type in CEL expressions.
		// In CEL, the type is represented as dynamic value, which can be thought of as a union type of all types.
		// All type checking for XIntOrString is deferred to runtime, so all access to values of this type must
		// be guarded with a type check, e.g.:
		//
		// To require that the string representation be a percentage:
		//  `type(intOrStringField) == string && intOrStringField.matches(r'(\d+(\.\d+)?%)')`
		// To validate requirements on both the int and string representation:
		//  `type(intOrStringField) == int ? intOrStringField < 5 : double(intOrStringField.replace('%', '')) < 0.5
		//
		dyn := newSimpleTypeWithMinSize("dyn", cel.DynType, nil, 1) // smallest value for a serialied x-kubernetes-int-or-string is 0
		// handle x-kubernetes-int-or-string by returning the max length/min serialized size of the largest possible string
		dyn.MaxElements = maxRequestSizeBytes - 2
		return dyn
	}

	// We ignore XPreserveUnknownFields since we don't support validation rules on
	// data that we don't have schema information for.

	if isResourceRoot {
		// 'apiVersion', 'kind', 'metadata.name' and 'metadata.generateName' are always accessible to validator rules
		// at the root of resources, even if not specified in the schema.
		// This includes the root of a custom resource and the root of XEmbeddedResource objects.
		s = WithTypeAndObjectMeta(s)
	}

	switch s.Type {
	case "array":
		if s.Items != nil {
			itemsType := SchemaDeclType(s.Items, s.Items.XEmbeddedResource)
			if itemsType == nil {
				return nil
			}
			var maxItems int64
			if s.ValueValidation != nil && s.ValueValidation.MaxItems != nil {
				maxItems = zeroIfNegative(*s.ValueValidation.MaxItems)
			} else {
				maxItems = estimateMaxArrayItemsFromMinSize(itemsType.MinSerializedSize)
			}
			return NewListType(itemsType, maxItems)
		}
		return nil
	case "object":
		if s.AdditionalProperties != nil && s.AdditionalProperties.Structural != nil {
			propsType := SchemaDeclType(s.AdditionalProperties.Structural, s.AdditionalProperties.Structural.XEmbeddedResource)
			if propsType != nil {
				var maxProperties int64
				if s.ValueValidation != nil && s.ValueValidation.MaxProperties != nil {
					maxProperties = zeroIfNegative(*s.ValueValidation.MaxProperties)
				} else {
					maxProperties = estimateMaxAdditionalPropertiesFromMinSize(propsType.MinSerializedSize)
				}
				return NewMapType(StringType, propsType, maxProperties)
			}
			return nil
		}
		fields := make(map[string]*DeclField, len(s.Properties))

		required := map[string]bool{}
		if s.ValueValidation != nil {
			for _, f := range s.ValueValidation.Required {
				required[f] = true
			}
		}
		// an object will always be serialized at least as {}, so account for that
		minSerializedSize := int64(2)
		for name, prop := range s.Properties {
			var enumValues []interface{}
			if prop.ValueValidation != nil {
				for _, e := range prop.ValueValidation.Enum {
					enumValues = append(enumValues, e.Object)
				}
			}
			if fieldType := SchemaDeclType(&prop, prop.XEmbeddedResource); fieldType != nil {
				if propName, ok := Escape(name); ok {
					fields[propName] = &DeclField{
						Name:         propName,
						Required:     required[name],
						Type:         fieldType,
						defaultValue: prop.Default.Object,
						enumValues:   enumValues, // Enum values are represented as strings in CEL
					}
				}
				// the min serialized size for an object is 2 (for {}) plus the min size of all its required
				// properties
				// only include required properties without a default value; default values are filled in
				// server-side
				if required[name] && prop.Default.Object == nil {
					minSerializedSize += int64(len(name)) + fieldType.MinSerializedSize + 4
				}
			}
		}
		objType := NewObjectType("object", fields)
		objType.MinSerializedSize = minSerializedSize
		return objType
	case "string":
		if s.ValueValidation != nil {
			switch s.ValueValidation.Format {
			case "byte":
				byteWithMaxLength := newSimpleTypeWithMinSize("bytes", cel.BytesType, types.Bytes([]byte{}), minStringSize)
				if s.ValueValidation.MaxLength != nil {
					byteWithMaxLength.MaxElements = zeroIfNegative(*s.ValueValidation.MaxLength)
				} else {
					byteWithMaxLength.MaxElements = estimateMaxStringLengthPerRequest(s)
				}
				return byteWithMaxLength
			case "duration":
				durationWithMaxLength := newSimpleTypeWithMinSize("duration", cel.DurationType, types.Duration{Duration: time.Duration(0)}, int64(minDurationSizeJSON))
				durationWithMaxLength.MaxElements = estimateMaxStringLengthPerRequest(s)
				return durationWithMaxLength
			case "date":
				timestampWithMaxLength := newSimpleTypeWithMinSize("timestamp", cel.TimestampType, types.Timestamp{Time: time.Time{}}, int64(dateSizeJSON))
				timestampWithMaxLength.MaxElements = estimateMaxStringLengthPerRequest(s)
				return timestampWithMaxLength
			case "date-time":
				timestampWithMaxLength := newSimpleTypeWithMinSize("timestamp", cel.TimestampType, types.Timestamp{Time: time.Time{}}, int64(minDatetimeSizeJSON))
				timestampWithMaxLength.MaxElements = estimateMaxStringLengthPerRequest(s)
				return timestampWithMaxLength
			}
		}
		strWithMaxLength := newSimpleTypeWithMinSize("string", cel.StringType, types.String(""), minStringSize)
		if s.ValueValidation != nil && s.ValueValidation.MaxLength != nil {
			// multiply the user-provided max length by 4 in the case of an otherwise-untyped string
			// we do this because the OpenAPIv3 spec indicates that maxLength is specified in runes/code points,
			// but we need to reason about length for things like request size, so we use bytes in this code (and an individual
			// unicode code point can be up to 4 bytes long)
			strWithMaxLength.MaxElements = zeroIfNegative(*s.ValueValidation.MaxLength) * 4
		} else {
			strWithMaxLength.MaxElements = estimateMaxStringLengthPerRequest(s)
		}
		return strWithMaxLength
	case "boolean":
		return BoolType
	case "number":
		return DoubleType
	case "integer":
		return IntType
	}
	return nil
}

func zeroIfNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// WithTypeAndObjectMeta ensures the kind, apiVersion and
// metadata.name and metadata.generateName properties are specified, making a shallow copy of the provided schema if needed.
func WithTypeAndObjectMeta(s *schema.Structural) *schema.Structural {
	if s.Properties != nil &&
		s.Properties["kind"].Type == "string" &&
		s.Properties["apiVersion"].Type == "string" &&
		s.Properties["metadata"].Type == "object" &&
		s.Properties["metadata"].Properties != nil &&
		s.Properties["metadata"].Properties["name"].Type == "string" &&
		s.Properties["metadata"].Properties["generateName"].Type == "string" {
		return s
	}
	result := &schema.Structural{
		Generic:         s.Generic,
		Extensions:      s.Extensions,
		ValueValidation: s.ValueValidation,
	}
	props := make(map[string]schema.Structural, len(s.Properties))
	for k, prop := range s.Properties {
		props[k] = prop
	}
	stringType := schema.Structural{Generic: schema.Generic{Type: "string"}}
	props["kind"] = stringType
	props["apiVersion"] = stringType
	props["metadata"] = schema.Structural{
		Generic: schema.Generic{Type: "object"},
		Properties: map[string]schema.Structural{
			"name":         stringType,
			"generateName": stringType,
		},
	}
	result.Properties = props

	return result
}

// MaxCardinality returns the maximum number of times data conforming to the minimum size given could possibly exist in
// an object serialized to JSON. For cases where a schema is contained under map or array schemas of unbounded
// size, this can be used as an estimate as the worst case number of times data matching the schema could be repeated.
// Note that this only assumes a single comma between data elements, so if the schema is contained under only maps,
// this estimates a higher cardinality that would be possible. DeclType.MinSerializedSize is meant to be passed to
// this function.
func MaxCardinality(minSize int64) uint64 {
	sz := minSize + 1 // assume at least one comma between elements
	return uint64(maxRequestSizeBytes / sz)
}

// estimateMaxStringLengthPerRequest estimates the maximum string length (in characters)
// of a string compatible with the format requirements in the provided schema.
// must only be called on schemas of type "string" or x-kubernetes-int-or-string: true
func estimateMaxStringLengthPerRequest(s *schema.Structural) int64 {
	if s.ValueValidation == nil || s.XIntOrString {
		// subtract 2 to account for ""
		return (maxRequestSizeBytes - 2)
	}
	switch s.ValueValidation.Format {
	case "duration":
		return maxDurationSizeJSON
	case "date":
		return dateSizeJSON
	case "date-time":
		return maxDatetimeSizeJSON
	default:
		// subtract 2 to account for ""
		return (maxRequestSizeBytes - 2)
	}
}

// estimateMaxArrayItemsPerRequest estimates the maximum number of array items with
// the provided minimum serialized size that can fit into a single request.
func estimateMaxArrayItemsFromMinSize(minSize int64) int64 {
	// subtract 2 to account for [ and ]
	return (maxRequestSizeBytes - 2) / (minSize + 1)
}

// estimateMaxAdditionalPropertiesPerRequest estimates the maximum number of additional properties
// with the provided minimum serialized size that can fit into a single request.
func estimateMaxAdditionalPropertiesFromMinSize(minSize int64) int64 {
	// 2 bytes for key + "" + colon + comma + smallest possible value, realistically the actual keys
	// will all vary in length
	keyValuePairSize := minSize + 6
	// subtract 2 to account for { and }
	return (maxRequestSizeBytes - 2) / keyValuePairSize
}
