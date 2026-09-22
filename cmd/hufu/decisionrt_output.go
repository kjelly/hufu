package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/kjelly/hufu/internal/decisionrt"
)

type decisionRTReceiptEnvelope struct {
	Result  decisionrt.Result  `json:"result"`
	Receipt decisionrt.Receipt `json:"receipt"`
}

type decisionRTValidationOutput struct {
	Valid         bool   `json:"valid"`
	RequestDigest string `json:"request_digest"`
}

type decisionRTBackendsOutput struct {
	Backends []BackendInfo `json:"backends"`
}

func writeDecisionRTResult(writer io.Writer, result decisionrt.Result, receipt decisionrt.Receipt, jsonOutput, includeReceipt bool) error {
	if includeReceipt {
		return writeDecisionRTJSON(writer, decisionRTReceiptEnvelope{Result: result, Receipt: receipt})
	}
	if jsonOutput {
		return writeDecisionRTJSON(writer, result)
	}
	var output string
	if result.Status == decisionrt.StatusAbstained {
		output = "abstained"
	} else {
		switch {
		case result.Value.Choice != "":
			output = result.Value.Choice
		case result.Value.Boolean != nil:
			output = strconv.FormatBool(*result.Value.Boolean)
		case result.Value.Integer != nil:
			output = strconv.FormatInt(*result.Value.Integer, 10)
		default:
			return decisionRTOutputError(errors.New("decision result has no value"))
		}
	}
	return writeDecisionRTBytes(writer, []byte(output+"\n"))
}

func writeDecisionRTValidation(writer io.Writer, digest string, jsonOutput bool) error {
	if jsonOutput {
		return writeDecisionRTJSON(writer, decisionRTValidationOutput{Valid: true, RequestDigest: digest})
	}
	return writeDecisionRTBytes(writer, []byte("valid "+digest+"\n"))
}

func writeDecisionRTBackends(writer io.Writer, backends []BackendInfo, jsonOutput bool) error {
	if jsonOutput {
		return writeDecisionRTJSON(writer, decisionRTBackendsOutput{Backends: backends})
	}
	var output bytes.Buffer
	_, _ = fmt.Fprintln(&output, "NAME\tAVAILABLE\tTYPE\tREASON")
	for _, backend := range backends {
		available := "no"
		if backend.Available {
			available = "yes"
		}
		reason := backend.Reason
		if reason == "" {
			reason = "-"
		}
		_, _ = fmt.Fprintf(&output, "%s\t%s\t%s\t%s\n", backend.Name, available, backend.Type, reason)
	}
	return writeDecisionRTBytes(writer, output.Bytes())
}

func writeDecisionRTJSON(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return decisionRTOutputError(err)
	}
	data = append(data, '\n')
	return writeDecisionRTBytes(writer, data)
}

func writeDecisionRTBytes(writer io.Writer, data []byte) error {
	written, err := writer.Write(data)
	if err != nil || written != len(data) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return decisionRTOutputError(err)
	}
	return nil
}

func decisionRTOutputError(cause error) error {
	return &decisionrt.RuntimeError{Kind: decisionrt.ErrorBackendFailure, Err: cause}
}
