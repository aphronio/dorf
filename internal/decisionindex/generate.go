package decisionindex

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type decision struct {
	ID            string
	Number        int
	Title         string
	Applicability string
	Areas         []string
	ReadWhen      string
	Filename      string
}

type area struct {
	Name    string
	Heading string
}

var (
	decisionFilename = regexp.MustCompile(`^(D[0-9]{3})-[a-z0-9]+(?:-[a-z0-9]+)*\.md$`)
	decisionHeading  = regexp.MustCompile(`^# (D[0-9]{3}): (.+)$`)
	areas            = []area{
		{Name: "product", Heading: "Product direction"},
		{Name: "core", Heading: "Core custody"},
		{Name: "workflows", Heading: "Workflows"},
		{Name: "interaction", Heading: "Interaction"},
		{Name: "sandboxes", Heading: "Sandboxes and profiles"},
		{Name: "harnesses", Heading: "Harnesses"},
		{Name: "model-access", Heading: "Model access"},
		{Name: "client-api", Heading: "Clients and API"},
		{Name: "deployment", Heading: "Deployment and setup"},
		{Name: "persistence", Heading: "Persistence"},
		{Name: "github", Heading: "GitHub integration"},
		{Name: "release", Heading: "Release and distribution"},
	}
)

// Source reads authoritative decision records and returns the two generated indexes.
func Source(sourceDir string) ([]byte, []byte, error) {
	decisions, err := loadDecisions(sourceDir)
	if err != nil {
		return nil, nil, err
	}
	return renderCurrent(decisions), renderHistorical(decisions), nil
}

func loadDecisions(sourceDir string) ([]decision, error) {
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("read decision directory: %w", err)
	}
	var decisions []decision
	for _, entry := range entries {
		if entry.Name() == "archive.md" {
			continue
		}
		if entry.IsDir() || !decisionFilename.MatchString(entry.Name()) {
			return nil, fmt.Errorf(
				"unexpected entry %s in decision directory; decision filenames must match DNNN-stable-slug.md",
				entry.Name(),
			)
		}
		parsed, err := parseDecision(filepath.Join(sourceDir, entry.Name()), entry.Name())
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, parsed)
	}
	if len(decisions) == 0 {
		return nil, fmt.Errorf("no decision records found in %s", sourceDir)
	}
	sort.Slice(decisions, func(i, j int) bool { return decisions[i].Number < decisions[j].Number })
	for index, item := range decisions {
		expected := index + 1
		if item.Number != expected {
			return nil, fmt.Errorf("decision sequence has %s where D%03d is required", item.ID, expected)
		}
	}
	return decisions, nil
}

func parseDecision(path, filename string) (decision, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return decision{}, fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) == 0 {
		return decision{}, fmt.Errorf("%s is empty", path)
	}
	heading := decisionHeading.FindStringSubmatch(lines[0])
	if heading == nil {
		return decision{}, fmt.Errorf("%s must start with '# DNNN: title'", path)
	}
	filenameID := decisionFilename.FindStringSubmatch(filename)[1]
	if heading[1] != filenameID {
		return decision{}, fmt.Errorf("%s heading ID %s does not match filename ID %s", path, heading[1], filenameID)
	}
	number, err := strconv.Atoi(strings.TrimPrefix(heading[1], "D"))
	if err != nil {
		return decision{}, fmt.Errorf("parse decision ID in %s: %w", path, err)
	}
	fields, err := metadata(lines, path)
	if err != nil {
		return decision{}, err
	}
	item := decision{
		ID:            heading[1],
		Number:        number,
		Title:         heading[2],
		Applicability: fields["Applicability"],
		ReadWhen:      fields["Read when"],
		Filename:      filename,
	}
	if strings.ContainsAny(item.Title, "[]|") {
		return decision{}, fmt.Errorf("%s title contains unsupported Markdown punctuation", path)
	}
	if err := validateApplicability(item.Applicability, path); err != nil {
		return decision{}, err
	}
	item.Areas, err = validateAreas(fields["Areas"], path)
	if err != nil {
		return decision{}, err
	}
	if !strings.HasSuffix(item.ReadWhen, ".") || strings.ContainsAny(item.ReadWhen, "|\n") {
		return decision{}, fmt.Errorf("%s Read when must be one table-safe sentence ending in a period", path)
	}
	return item, nil
}

func metadata(lines []string, path string) (map[string]string, error) {
	wanted := []string{"Applicability", "Areas", "Read when", "Decision history"}
	fields := make(map[string]string, len(wanted))
	for _, line := range lines[1:] {
		for _, name := range wanted {
			prefix := "- **" + name + ":** "
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			if _, exists := fields[name]; exists {
				return nil, fmt.Errorf("%s repeats %s metadata", path, name)
			}
			fields[name] = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	for _, name := range wanted {
		if fields[name] == "" || fields[name] == "TODO" {
			return nil, fmt.Errorf("%s has no %s metadata", path, name)
		}
	}
	return fields, nil
}

func validateApplicability(value, path string) error {
	switch value {
	case "current", "partial", "historical":
		return nil
	default:
		return fmt.Errorf("%s has invalid Applicability %q", path, value)
	}
}

func validateAreas(value, path string) ([]string, error) {
	allowed := make(map[string]struct{}, len(areas))
	for _, item := range areas {
		allowed[item.Name] = struct{}{}
	}
	values := strings.Split(value, ",")
	if len(values) == 0 || len(values) > 3 {
		return nil, fmt.Errorf("%s must name one to three Areas", path)
	}
	seen := make(map[string]struct{}, len(values))
	for index := range values {
		values[index] = strings.TrimSpace(values[index])
		if _, ok := allowed[values[index]]; !ok {
			return nil, fmt.Errorf("%s has invalid Area %q", path, values[index])
		}
		if _, exists := seen[values[index]]; exists {
			return nil, fmt.Errorf("%s repeats Area %q", path, values[index])
		}
		seen[values[index]] = struct{}{}
	}
	return values, nil
}

func renderCurrent(decisions []decision) []byte {
	var output bytes.Buffer
	output.WriteString("<!-- Code generated by dorf-decision-index. DO NOT EDIT. -->\n")
	output.WriteString("# Dorf decision guide\n\n")
	output.WriteString("Start with the [documentation map](../README.md) and the current authority for the work. ")
	output.WriteString("Use this guide when you need the rationale behind a current boundary.\n\n")
	output.WriteString("Choose the section for the boundary you are changing, scan the `Read when` triggers, and open only matching records. ")
	output.WriteString("If a task names a decision ID, search this guide for that ID. A record may appear under several areas, but its linked file remains the single source.\n\n")
	output.WriteString("Each linked file keeps routing metadata next to the authoritative decision and its history. ")
	output.WriteString("Follow the [decision procedure](../../CONTRIBUTING.md#record-a-decision) when a choice changes.\n\n")
	output.WriteString("`current` decisions still govern. `partial` decisions retain a boundary that a later decision changed in part.\n")
	for _, area := range areas {
		writeArea(&output, area, decisions)
	}
	output.WriteString("\n## Historical decisions\n\n")
	output.WriteString("Open the [historical decision index](decisions/archive.md) only when you need replaced rationale or a supersession chain.\n")
	return output.Bytes()
}

func writeArea(output *bytes.Buffer, group area, decisions []decision) {
	var matches []decision
	for _, item := range decisions {
		if item.Applicability == "historical" || !contains(item.Areas, group.Name) {
			continue
		}
		matches = append(matches, item)
	}
	if len(matches) == 0 {
		return
	}
	fmt.Fprintf(output, "\n## %s\n\n", group.Heading)
	for _, item := range matches {
		fmt.Fprintf(
			output,
			"- [%s: %s](decisions/%s) (`%s`). %s\n",
			item.ID,
			item.Title,
			item.Filename,
			item.Applicability,
			item.ReadWhen,
		)
	}
}

func renderHistorical(decisions []decision) []byte {
	var output bytes.Buffer
	output.WriteString("<!-- Code generated by dorf-decision-index. DO NOT EDIT. -->\n")
	output.WriteString("# Historical Dorf decisions\n\n")
	output.WriteString("These decisions no longer govern current work. Read one when a current decision links to it or when you need the rationale behind a replaced design.\n\n")
	output.WriteString("Return to the [current decision guide](../decisions.md).\n\n")
	for _, item := range decisions {
		if item.Applicability != "historical" {
			continue
		}
		fmt.Fprintf(
			&output,
			"- [%s: %s](%s). Areas: %s. %s\n",
			item.ID,
			item.Title,
			item.Filename,
			strings.Join(item.Areas, ", "),
			item.ReadWhen,
		)
	}
	return output.Bytes()
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
