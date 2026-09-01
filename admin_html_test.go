package main

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func htmlAttribute(node *html.Node, name string) string {
	for _, attribute := range node.Attr {
		if attribute.Key == name {
			return attribute.Val
		}
	}
	return ""
}

func TestAdminHTMLUsesSemanticNavigationAndLiveFeedback(t *testing.T) {
	document, err := html.Parse(strings.NewReader(adminHTML))
	if err != nil {
		t.Fatalf("parse admin HTML: %v", err)
	}
	mainCount := 0
	navigationButtons := 0
	liveToast := false
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			if node.Data == "main" && htmlAttribute(node, "id") == "mainContent" {
				mainCount++
			}
			if node.Data == "button" && strings.Contains(" "+htmlAttribute(node, "class")+" ", " nav-item ") {
				navigationButtons++
			}
			if htmlAttribute(node, "id") == "toast" && htmlAttribute(node, "aria-live") == "polite" {
				liveToast = true
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	if mainCount != 1 || navigationButtons < 8 || !liveToast {
		t.Fatalf("admin semantics: main=%d navigationButtons=%d liveToast=%v", mainCount, navigationButtons, liveToast)
	}
	if strings.Contains(adminHTML, "transition:all") {
		t.Fatal("admin CSS still contains transition:all")
	}
	for _, unsafe := range []string{
		`<div class="nav-item`,
		`value="' + esc(`,
		`title="' + esc(model.id)`,
		`onclick="deleteModel(\'' + esc(`,
	} {
		if strings.Contains(adminHTML, unsafe) {
			t.Fatalf("admin HTML contains context-unsafe dynamic markup %q", unsafe)
		}
	}
}
