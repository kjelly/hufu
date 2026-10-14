// Package resultquery resolves natural-language history questions into bounded,
// validated read queries. It never executes tools or changes task state.
package resultquery

import (
	"encoding/json/v2"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const planSchema = `{
 "$schema":"https://json-schema.org/draft/2020-12/schema", "type":"object", "additionalProperties":false,
 "required":["schema_version","resolution","intent","selectors","time_range","statuses","status_basis","search_terms","order","limit","clarification"],
 "properties":{
  "schema_version":{"const":1},
  "resolution":{"enum":["resolved","needs_clarification","unsupported"]},
  "intent":{"enum":["find","summarize","compare"]},
  "selectors":{"type":"object","additionalProperties":false,"required":["team","agent","invocation_id","run_id"],"properties":{
   "team":{"type":"string","maxLength":128},"agent":{"type":"string","maxLength":128},
   "invocation_id":{"type":"string","maxLength":256},"run_id":{"type":"string","maxLength":256}}},
  "time_range":{"type":"object","additionalProperties":false,"required":["since","until","timezone"],"properties":{
   "since":{"type":"string","maxLength":64},"until":{"type":"string","maxLength":64},"timezone":{"type":"string","maxLength":128}}},
  "statuses":{"type":"array","maxItems":5,"uniqueItems":true,"items":{"enum":["done","error","blocked","skipped","protocol_incomplete"]}},
  "status_basis":{"enum":["at_terminal","current"]},
  "search_terms":{"type":"array","maxItems":3,"uniqueItems":true,"items":{"type":"string","minLength":1,"maxLength":256}},
  "order":{"enum":["relevance","newest","oldest"]},"limit":{"type":"integer","minimum":1,"maximum":50},
  "clarification":{"type":"string","maxLength":1000}
 }} `

const selectionSchema = `{"type":"object","additionalProperties":false,"required":["result_ids"],"properties":{
 "result_ids":{"type":"array","maxItems":10,"uniqueItems":true,"items":{"type":"string","minLength":1,"maxLength":128}}
}}`

const answerSchema = `{"type":"object","additionalProperties":false,"required":["blocks"],"properties":{
 "blocks":{"type":"array","maxItems":20,"items":{"type":"object","additionalProperties":false,"required":["kind","text","sources"],"properties":{
  "kind":{"enum":["fact","inference","unavailable"]},"text":{"type":"string","minLength":1,"maxLength":2000},
  "sources":{"type":"array","maxItems":10,"uniqueItems":true,"items":{"type":"string","minLength":1,"maxLength":128}}
 }}}
}}`

var compiledSchemas = sync.OnceValues(func() (map[string]*jsonschema.Schema, error) {
	result := make(map[string]*jsonschema.Schema)
	for name, raw := range map[string]string{"plan": planSchema, "selection": selectionSchema, "answer": answerSchema} {
		var document any
		if err := json.Unmarshal([]byte(raw), &document); err != nil {
			return nil, err
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource(name+".json", document); err != nil {
			return nil, err
		}
		schema, err := compiler.Compile(name + ".json")
		if err != nil {
			return nil, err
		}
		result[name] = schema
	}
	return result, nil
})

func decodeResponse(raw, name string, output any) error {
	if len(raw) > 64<<10 {
		return fmt.Errorf("%s response exceeds 64 KiB", name)
	}
	var document any
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		return fmt.Errorf("%s response must be one strict JSON object: %w", name, err)
	}
	schemas, err := compiledSchemas()
	if err != nil {
		return err
	}
	if err := schemas[name].Validate(document); err != nil {
		return fmt.Errorf("%s response violates its schema", name)
	}
	if err := json.Unmarshal([]byte(raw), output, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid %s response: %w", name, err)
	}
	return nil
}
