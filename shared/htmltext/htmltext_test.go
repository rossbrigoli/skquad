package htmltext

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractBasics(t *testing.T) {
	doc := `<!DOCTYPE html>
<html>
<head>
  <title>My Page</title>
  <style>body { color: red; }</style>
  <script>var x = "<h1>injected</h1>";</script>
</head>
<body>
  <!-- a comment -->
  <h1>Hello</h1>
  <p>First   paragraph.</p>
  <p>Second &amp; third &quot;quoted&quot; &#39;bits&#39; &nbsp;here.</p>
  <div><ul><li>one</li><li>two</li></ul></div>
  <script>evil()</script>
</body>
</html>`
	out := Extract(doc)
	require.Contains(t, out, "My Page")
	require.Contains(t, out, "Hello")
	require.Contains(t, out, "First paragraph.")
	require.Contains(t, out, `Second & third "quoted" 'bits' here.`)
	require.Contains(t, out, "one")
	require.Contains(t, out, "two")
	require.NotContains(t, out, "color: red")
	require.NotContains(t, out, "evil()")
	require.NotContains(t, out, "injected")
	require.NotContains(t, out, "a comment")
	// No glued words across block boundaries.
	require.NotContains(t, out, "HelloFirst")
	// Whitespace collapsed: no double spaces anywhere.
	require.NotRegexp(t, `  `, out)
	// Block boundaries produce multiple lines.
	require.Greater(t, len(strings.Split(out, "\n")), 3)
}

func TestExtractEmpty(t *testing.T) {
	require.Equal(t, "", Extract(""))
	require.Equal(t, "", Extract("   \n\t "))
}

func TestExtractMalformedDegrades(t *testing.T) {
	// Unterminated tags/comments: keep text, never panic.
	out := Extract("<p>hello <div world")
	require.Contains(t, out, "hello")
	out = Extract("<p>hi <!-- unterminated")
	require.Contains(t, out, "hi")
	out = Extract("<<<>>>")
	require.NotContains(t, out, "<")
}

func TestExtractSkipsNestedScriptStyle(t *testing.T) {
	doc := `<body><div><script><style>x</style>y</script>visible</div></body>`
	out := Extract(doc)
	require.Contains(t, out, "visible")
	require.NotContains(t, out, "y")
	require.NotContains(t, out, "x")
}

func TestExtractNumericEntities(t *testing.T) {
	out := Extract("<p>&#65;&#x42;&lt;tag&gt;</p>")
	require.Contains(t, out, "AB<tag>")
}
