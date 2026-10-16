// Command webdict writes the trimmed FIX data dictionary the Web UI's FIX
// parser uses (design doc, section 11.4): field names and enum
// descriptions, message names, and the repeating groups of each message.
//
//	go run ./cmd/webdict -spec spec/FIX44.xml -o web/src/fix/dict.json
package main

import (
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"strconv"
)

type xmlSpec struct {
	Messages   []xmlMessage   `xml:"messages>message"`
	Components []xmlComponent `xml:"components>component"`
	Fields     []xmlField     `xml:"fields>field"`
}

type xmlMessage struct {
	Name    string    `xml:"name,attr"`
	MsgType string    `xml:"msgtype,attr"`
	Items   []xmlItem `xml:",any"`
}

type xmlComponent struct {
	Name  string    `xml:"name,attr"`
	Items []xmlItem `xml:",any"`
}

// xmlItem is a field, group or component reference inside a message,
// component or group.
type xmlItem struct {
	XMLName xml.Name
	Name    string    `xml:"name,attr"`
	Items   []xmlItem `xml:",any"`
}

type xmlField struct {
	Number int    `xml:"number,attr"`
	Name   string `xml:"name,attr"`
	Type   string `xml:"type,attr"`
	Values []struct {
		Enum        string `xml:"enum,attr"`
		Description string `xml:"description,attr"`
	} `xml:"value"`
}

type field struct {
	Name  string            `json:"n"`
	Type  string            `json:"t"`
	Enums map[string]string `json:"e,omitempty"`
}

// group describes a repeating group: its delimiter (first) tag and every
// tag that may appear in an entry, nested groups included.
type group struct {
	Delim   int   `json:"d"`
	Members []int `json:"m"`
}

type dict struct {
	Fields   map[string]field            `json:"fields"`
	Messages map[string]string           `json:"messages"`
	Groups   map[string]map[string]group `json:"groups"` // msgType -> count tag -> group
}

func main() {
	specPath := flag.String("spec", "spec/FIX44.xml", "FIX data dictionary")
	out := flag.String("o", "", "output file (default stdout)")
	flag.Parse()

	data, err := os.ReadFile(*specPath)
	if err != nil {
		fail(err)
	}
	var spec xmlSpec
	if err := xml.Unmarshal(data, &spec); err != nil {
		fail(err)
	}

	d := dict{Fields: map[string]field{}, Messages: map[string]string{}, Groups: map[string]map[string]group{}}
	tags := map[string]int{}
	for _, f := range spec.Fields {
		tags[f.Name] = f.Number
		fd := field{Name: f.Name, Type: f.Type}
		if len(f.Values) > 0 {
			fd.Enums = map[string]string{}
			for _, v := range f.Values {
				fd.Enums[v.Enum] = v.Description
			}
		}
		d.Fields[strconv.Itoa(f.Number)] = fd
	}
	components := map[string][]xmlItem{}
	for _, c := range spec.Components {
		components[c.Name] = c.Items
	}

	r := resolver{tags: tags, components: components}
	for _, m := range spec.Messages {
		d.Messages[m.MsgType] = m.Name
		groups := map[string]group{}
		r.collectGroups(m.Items, groups)
		if len(groups) > 0 {
			d.Groups[m.MsgType] = groups
		}
	}

	enc, err := json.Marshal(d)
	if err != nil {
		fail(err)
	}
	if *out == "" {
		os.Stdout.Write(enc)
		return
	}
	if err := os.WriteFile(*out, enc, 0o644); err != nil {
		fail(err)
	}
}

type resolver struct {
	tags       map[string]int
	components map[string][]xmlItem
}

// expand inlines component references.
func (r resolver) expand(items []xmlItem) []xmlItem {
	var out []xmlItem
	for _, it := range items {
		if it.XMLName.Local == "component" {
			out = append(out, r.expand(r.components[it.Name])...)
			continue
		}
		out = append(out, it)
	}
	return out
}

// members lists every tag an entry made of items may carry.
func (r resolver) members(items []xmlItem) []int {
	var out []int
	for _, it := range r.expand(items) {
		out = append(out, r.tags[it.Name])
		if it.XMLName.Local == "group" {
			out = append(out, r.members(it.Items)...)
		}
	}
	return out
}

func (r resolver) collectGroups(items []xmlItem, groups map[string]group) {
	for _, it := range r.expand(items) {
		if it.XMLName.Local != "group" {
			continue
		}
		entry := r.expand(it.Items)
		if len(entry) == 0 {
			continue
		}
		groups[strconv.Itoa(r.tags[it.Name])] = group{Delim: r.tags[entry[0].Name], Members: r.members(it.Items)}
		r.collectGroups(it.Items, groups)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "webdict:", err)
	os.Exit(1)
}
