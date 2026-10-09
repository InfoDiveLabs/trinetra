package web

import (
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// formValuesForButton parses a rendered page (body), finds the <form> that
// contains a submit control named btnName with value btnValue (a <button>
// or an <input type="submit">), and returns exactly the fields a browser
// would submit for THAT form when THAT button is clicked: every enabled
// <input>/<select>/<textarea> inside it (following normal HTML form-
// submission semantics -- a disabled field is never submitted, an unchecked
// checkbox/radio is never submitted, a <select> with no option explicitly
// selected defaults to its first <option>), plus the clicked button's own
// name=value pair. Every OTHER button/submit control in the form is
// excluded, exactly like a real click only ever submits the one control
// that was activated.
//
// This lets a test post precisely what the browser would, catching a button
// living in one <form> that silently drops ANOTHER section's fields because
// they live in a different <form> -- which a hand-built url.Values fixture
// can't catch, since it reflects what the test AUTHOR assumed the form
// contained, not what the rendered HTML actually wired up.
func formValuesForButton(t *testing.T, body, btnName, btnValue string) url.Values {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("formValuesForButton: parse HTML: %v", err)
	}
	form := findFormWithButton(doc, btnName, btnValue)
	if form == nil {
		t.Fatalf("formValuesForButton: no <form> found containing a submit control name=%q value=%q", btnName, btnValue)
	}
	values := collectFormValues(form)
	if btnName != "" {
		values.Add(btnName, btnValue)
	}
	return values
}

// htmlAttr reads attribute key off n, reporting whether it was present at
// all (a bare boolean attribute like `disabled`/`checked`/`selected` has an
// empty Val but IS present).
func htmlAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// isSubmitControl reports whether n is a <button> or <input type=submit>
// (the two elements a real browser lets you click to submit a form).
func isSubmitControl(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if n.Data == "button" {
		typ, has := htmlAttr(n, "type")
		return !has || strings.EqualFold(typ, "submit")
	}
	if n.Data == "input" {
		typ, _ := htmlAttr(n, "type")
		return strings.EqualFold(typ, "submit")
	}
	return false
}

// nodeMatchesButton reports whether submit control n carries exactly
// name=btnName value=btnValue (both attributes read literally -- a button
// with no name attribute at all only matches btnName=="").
func nodeMatchesButton(n *html.Node, btnName, btnValue string) bool {
	if !isSubmitControl(n) {
		return false
	}
	name, _ := htmlAttr(n, "name")
	val, _ := htmlAttr(n, "value")
	return name == btnName && val == btnValue
}

// containsMatchingButton reports whether n or any descendant is a submit
// control matching (btnName, btnValue).
func containsMatchingButton(n *html.Node, btnName, btnValue string) bool {
	if nodeMatchesButton(n, btnName, btnValue) {
		return true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if containsMatchingButton(c, btnName, btnValue) {
			return true
		}
	}
	return false
}

// findFormWithButton does a depth-first search for the first <form> element
// whose subtree contains a submit control matching (btnName, btnValue).
// Forms don't nest in valid HTML, so the first one found containing the
// button is unambiguously the right one.
func findFormWithButton(n *html.Node, btnName, btnValue string) *html.Node {
	if n.Type == html.ElementNode && n.Data == "form" && containsMatchingButton(n, btnName, btnValue) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findFormWithButton(c, btnName, btnValue); found != nil {
			return found
		}
	}
	return nil
}

// nodeText concatenates every text-node descendant of n, in document order
// -- used for a <textarea>'s content and an <option>'s fallback value (an
// <option> with no value attribute submits its text content instead).
func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

// collectFormValues walks form's subtree collecting every enabled, named
// input/select/textarea's value exactly as a browser would submit it (see
// formValuesForButton's doc for the exact semantics). The clicked button's
// own name/value is NOT added here -- formValuesForButton adds it
// separately, since collectFormValues has no notion of which button (if
// any) was clicked.
func collectFormValues(form *html.Node) url.Values {
	values := url.Values{}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if _, disabled := htmlAttr(n, "disabled"); disabled {
				return
			}
			switch n.Data {
			case "input":
				name, hasName := htmlAttr(n, "name")
				if !hasName || name == "" {
					return
				}
				typ, _ := htmlAttr(n, "type")
				typ = strings.ToLower(typ)
				switch typ {
				case "submit", "button", "reset", "image", "file":
					return
				case "checkbox", "radio":
					if _, checked := htmlAttr(n, "checked"); !checked {
						return
					}
				}
				val, _ := htmlAttr(n, "value")
				values.Add(name, val)
				return
			case "textarea":
				name, hasName := htmlAttr(n, "name")
				if hasName && name != "" {
					values.Add(name, nodeText(n))
				}
				return
			case "select":
				name, hasName := htmlAttr(n, "name")
				if !hasName || name == "" {
					return
				}
				anySelected := false
				firstVal, haveFirst := "", false
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type != html.ElementNode || c.Data != "option" {
						continue
					}
					val, hasVal := htmlAttr(c, "value")
					if !hasVal {
						val = nodeText(c)
					}
					if !haveFirst {
						firstVal, haveFirst = val, true
					}
					if _, selected := htmlAttr(c, "selected"); selected {
						values.Add(name, val)
						anySelected = true
					}
				}
				if !anySelected && haveFirst {
					values.Add(name, firstVal)
				}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(form)
	return values
}
