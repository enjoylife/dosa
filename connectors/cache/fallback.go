package cache

import (
	"context"
	"errors"
	"sort"

	"github.com/uber-go/dosa"
	"github.com/uber-go/dosa/connectors/base"
)

const (
	key   = "key"
	value = "value"
)

type rangeResults struct {
	Rows      []map[string]dosa.FieldValue
	TokenNext string
}

type rangeQuery struct {
	Conditions map[string][]*dosa.Condition `json:",omitempty"`
	Token      string
	Limit      int
}

// NewConnector creates a fallback cache connector
func NewConnector(origin dosa.Connector, fallback dosa.Connector, encoder Encoder) *Connector {
	bc := base.Connector{Next: origin}
	return &Connector{
		Connector: bc,
		origin:    origin,
		fallback:  fallback,
		encoder:   encoder,
	}
}

// Connector is a fallback cache connector
type Connector struct {
	base.Connector
	origin   dosa.Connector
	fallback dosa.Connector
	encoder  Encoder
	// Used primarily for testing so that nothing is called in a goroutine
	synchronous bool
}

// Upsert dual writes to the fallback cache and the origin
func (c *Connector) Upsert(ctx context.Context, ei *dosa.EntityInfo, values map[string]dosa.FieldValue) error {
	w := func() error {
		cacheKey := createCacheKey(ei, values, c.encoder)
		cacheValue, _ := c.encoder.Encode(values)
		adaptedEi := adaptToKeyValue(ei)
		newValues := map[string]dosa.FieldValue{
			key:   cacheKey,
			value: cacheValue,
		}
		return c.fallback.Upsert(ctx, adaptedEi, newValues)
	}
	_ = c.cacheWrite(w)

	return c.origin.Upsert(ctx, ei, values)
}

func (c *Connector) Read(ctx context.Context, ei *dosa.EntityInfo, keys map[string]dosa.FieldValue, minimumFields []string) (values map[string]dosa.FieldValue, err error) {
	// Read from source of truth first
	source, sourceErr := c.origin.Read(ctx, ei, keys, dosa.All())

	cacheKey := createCacheKey(ei, keys, c.encoder)
	adaptedEi := adaptToKeyValue(ei)
	// if source of truth is good, return result and write result to cache
	if sourceErr == nil {
		w := func() error {
			cacheValue, _ := c.encoder.Encode(source)
			newValues := map[string]dosa.FieldValue{
				key:   cacheKey,
				value: cacheValue}
			return c.fallback.Upsert(ctx, adaptedEi, newValues)
		}
		_ = c.cacheWrite(w)

		return source, sourceErr
	}
	// if source of truth fails, try the fallback. If the fallback fails,
	// return the original error
	value, err := c.getValueFromFallback(ctx, adaptedEi, cacheKey)
	if err != nil {
		return source, sourceErr
	}
	result := map[string]dosa.FieldValue{}
	err = c.encoder.Decode(value, &result)
	if err != nil {
		return source, sourceErr
	}
	return result, err
}

// Range returns range from origin, reverts to fallback if origin fails
func (c *Connector) Range(ctx context.Context, ei *dosa.EntityInfo, columnConditions map[string][]*dosa.Condition, minimumFields []string, token string, limit int) ([]map[string]dosa.FieldValue, string, error) {
	sourceRows, sourceToken, sourceErr := c.origin.Range(ctx, ei, columnConditions, dosa.All(), token, limit)

	// TODO serializing dosa.Condition array? conditions could be any order
	keysMap := rangeQuery{
		Conditions: columnConditions,
		Token:      token,
		Limit:      limit,
	}
	cacheKey, _ := c.encoder.Encode(keysMap)
	adaptedEi := adaptToKeyValue(ei)

	if sourceErr == nil {
		w := func() error {
			rangeResults := rangeResults{
				TokenNext: sourceToken,
				Rows:      sourceRows,
			}
			cacheValue, _ := c.encoder.Encode(rangeResults)
			newValues := map[string]dosa.FieldValue{
				key:   cacheKey,
				value: cacheValue,
			}
			return c.fallback.Upsert(ctx, adaptedEi, newValues)
		}
		_ = c.cacheWrite(w)

		return sourceRows, sourceToken, sourceErr
	}
	value, err := c.getValueFromFallback(ctx, adaptedEi, cacheKey)
	if err != nil {
		return sourceRows, sourceToken, sourceErr
	}
	unpack := rangeResults{}
	err = c.encoder.Decode(value, &unpack)
	if err != nil {
		return sourceRows, sourceToken, sourceErr
	}
	return unpack.Rows, unpack.TokenNext, err
}

// Scan returns scan result from origin.
func (c *Connector) Scan(ctx context.Context, ei *dosa.EntityInfo, minimumFields []string, token string, limit int) ([]map[string]dosa.FieldValue, string, error) {
	// Scan will just call range with no conditions
	return c.Range(ctx, ei, nil, minimumFields, token, limit)
}

// Remove deletes an entry
func (c *Connector) Remove(ctx context.Context, ei *dosa.EntityInfo, keys map[string]dosa.FieldValue) error {
	w := func() error {
		cacheKey := createCacheKey(ei, keys, c.encoder)
		adaptedEi := adaptToKeyValue(ei)
		return c.fallback.Remove(ctx, adaptedEi, map[string]dosa.FieldValue{key: cacheKey})
	}
	_ = c.cacheWrite(w)

	return c.origin.Remove(ctx, ei, keys)
}

func (c *Connector) getValueFromFallback(ctx context.Context, ei *dosa.EntityInfo, keyValue []byte) ([]byte, error) {
	// if source of truth fails, try the fallback. If the fallback fails,
	// return the original error
	response, err := c.fallback.Read(ctx, ei, map[string]dosa.FieldValue{key: keyValue}, dosa.All())
	if err != nil {
		return nil, err
	}

	// unpack the value
	cacheValue, ok := response[value].([]byte)
	if !ok {
		return nil, errors.New("No value in cache for key")
	}
	return cacheValue, nil
}

func (c *Connector) setSynchronousMode(sync bool) {
	c.synchronous = sync
}

func (c *Connector) cacheWrite(w func() error) error {
	if c.synchronous {
		return w()
	}
	go func() { _ = w() }()
	return nil
}

func adaptToKeyValue(ei *dosa.EntityInfo) *dosa.EntityInfo {
	adaptedEi := &dosa.EntityInfo{}
	adaptedEi.Ref = ei.Ref
	adaptedEi.Def = &dosa.EntityDefinition{
		Name: ei.Def.Name,
		Key: &dosa.PrimaryKey{
			PartitionKeys: []string{key},
		},
		Columns: []*dosa.ColumnDefinition{
			{Name: value, Type: dosa.Blob},
			{Name: key, Type: dosa.Blob},
		},
	}
	return adaptedEi
}

// used for single entry reads/writes
func createCacheKey(ei *dosa.EntityInfo, values map[string]dosa.FieldValue, e Encoder) []byte {
	keys := []string{}
	for pk := range ei.Def.KeySet() {
		if _, ok := values[pk]; ok {
			keys = append(keys, pk)
		}
	}
	// sort the keys so that we encode in a specific order
	sort.Strings(keys)

	orderedKeys := []map[string]dosa.FieldValue{}
	for _, k := range keys {
		orderedKeys = append(orderedKeys, map[string]dosa.FieldValue{k: values[k]})
	}

	cacheKey, err := e.Encode(orderedKeys)
	if err != nil {
		return []byte{}
	}
	return cacheKey
}