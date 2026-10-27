package dynamodb

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/scanner"
)

// baselineTokens is a lexical guard, not a Terraform evaluator. Check all module
// files so moving a forbidden block to another file cannot bypass the baseline.
// Comments are ignored; dynamic block labels are checked as well as identifiers.
func baselineTokens(source string) ([]string, error) {
	var s scanner.Scanner
	s.Init(strings.NewReader(source))
	s.Mode = scanner.ScanIdents | scanner.ScanStrings | scanner.ScanComments | scanner.SkipComments
	var scanErr error
	s.Error = func(_ *scanner.Scanner, message string) { scanErr = fmt.Errorf("Terraform token scan: %s", message) }
	var tokens []string
	for token := s.Scan(); token != scanner.EOF; token = s.Scan() {
		if token == '#' {
			for s.Peek() != '\n' && s.Peek() != scanner.EOF {
				s.Next()
			}
			continue
		}
		tokens = append(tokens, s.TokenText())
	}
	return tokens, scanErr
}

func checkTerraformBaseline(sources []string) error {
	resourceCount := 0
	for _, source := range sources {
		tokens, err := baselineTokens(source)
		if err != nil {
			return err
		}
		for i, token := range tokens {
			candidate := token
			if i > 0 && tokens[i-1] == "dynamic" {
				candidate = strings.Trim(token, `"`)
			}
			switch candidate {
			case "global_secondary_index", "local_secondary_index", "ttl":
				return fmt.Errorf("W04 baseline forbids %s; a reviewed migration must update this guard", candidate)
			}
			if token == "resource" && i+1 < len(tokens) && tokens[i+1] == `"aws_dynamodb_table"` {
				resourceCount++
				// The single reviewed resource must enumerate the checked table map.
				if i+2 >= len(tokens) || tokens[i+2] != `"metadata"` {
					return fmt.Errorf("unexpected DynamoDB resource outside the baseline inventory")
				}
				depth, inventoryBound := 0, false
				for j := i + 3; j < len(tokens); j++ {
					if tokens[j] == "{" {
						depth++
					} else if tokens[j] == "}" {
						depth--
						if depth == 0 {
							break
						}
					}
					if depth == 1 && tokens[j] == "for_each" && j+5 < len(tokens) {
						// Require the entire expression, not a prefix such as
						// merge(local.tables, extra) or local.tables.extra.
						inventoryBound = strings.Join(tokens[j+1:j+5], " ") == "= local . tables" &&
							(tokens[j+5] == "}" || (j+6 < len(tokens) && tokens[j+6] == "="))
					}
				}
				if !inventoryBound {
					return fmt.Errorf("DynamoDB resource must enumerate exactly local.tables")
				}
			}
		}
	}
	if resourceCount != 1 {
		return fmt.Errorf("expected one DynamoDB resource enumerating the 20-table inventory, got %d", resourceCount)
	}
	return nil
}

func TestTerraformBaselineHasNoIndexesOrTTL(t *testing.T) {
	paths, err := filepath.Glob("../../../infra/terraform/cloud/modules/data/*.tf")
	if err != nil || len(paths) == 0 {
		t.Fatalf("Terraform module files: %v, %v", paths, err)
	}
	var sources []string
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, string(b))
	}
	if err := checkTerraformBaseline(sources); err != nil {
		t.Fatal(err)
	}
}

func TestTerraformBaselineGuardRejectsDrift(t *testing.T) {
	base := `resource "aws_dynamodb_table" "metadata" { for_each = local.tables }`
	for _, name := range []string{"global_secondary_index", "local_secondary_index", "ttl"} {
		for _, block := range []string{name + " {}", `dynamic "` + name + `" { for_each = [] content {} }`} {
			t.Run(block, func(t *testing.T) {
				if err := checkTerraformBaseline([]string{base, block}); err == nil {
					t.Fatal("forbidden block in another module file was accepted")
				}
			})
		}
	}
	for _, sources := range [][]string{nil, {base, `resource "aws_dynamodb_table" "extra" {}`}, {base, base},
		{`resource "aws_dynamodb_table" "metadata" { for_each = local.other }`},
		{`resource "aws_dynamodb_table" "metadata" { for_each = local.tables.extra }`}} {
		if err := checkTerraformBaseline(sources); err == nil {
			t.Fatal("missing or additional table resource was accepted")
		}
	}
	comments := "# ttl {}\n// global_secondary_index {}\n/* dynamic \"local_secondary_index\" {} */\n"
	if err := checkTerraformBaseline([]string{comments + base}); err != nil {
		t.Fatalf("comments changed baseline: %v", err)
	}
}
