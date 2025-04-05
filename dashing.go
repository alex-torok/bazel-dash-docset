package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/andybalholm/cascadia"
	css "github.com/andybalholm/cascadia"
	"github.com/urfave/cli/v2"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	_ "github.com/mattn/go-sqlite3"
	"gopkg.in/yaml.v3"
)

const plist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>{{.Name}}</string>
	<key>CFBundleName</key>
	<string>{{.FancyName}}</string>
	<key>DocSetPlatformFamily</key>
	<string>{{.Name}}</string>
	<key>isDashDocset</key>
	<true/>
	<key>DashDocSetFamily</key>
	<string>dashtoc3</string>
	<key>dashIndexFilePath</key>
	<string>{{.Index}}</string>
	<key>isJavaScriptEnabled</key><{{.AllowJS}}/>{{if .ExternalURL}}
	<key>DashDocSetFallbackURL</key>
	<string>{{.ExternalURL}}</string>{{end}}
</dict>
</plist>
`

// Automatically replaced by linker.
var version = "dev"

type Dashing struct {
	// The human-oriented name of the package.
	Name string `yaml:"name"`
	// Computer-readable name. Recommendation is to use one word.
	Package string `yaml:"package"`
	// The location of the index.html file.
	Index string `yaml:"index"`
	// Selectors to match.
	Selectors []Transform `yaml:"selectors"`
	// BackupSelectors - Used if none of the selectors match
	BackupSelectors []Transform `yaml:"backup_selectors"`
	// IgnorePathRegexes will cause any html file that matches from being pulled into the docs
	IgnorePathRegexes []*regexp.Regexp `yaml:"ignore_path_regexes"`
	// WalkRoot is the directory to start walking from.
	WalkRoot string `yaml:"walk_root"`
	// DocsRoot is the directory where the root of the http server is.
	DocsRoot string `yaml:"docs_root"`
	// CopyDirsIntoDocs is a list of directories to include in the docset.
	CopyDirsIntoDocs []string `yaml:"copy_dirs_into_docs"`
	// RemoveElements
	RemoveElements []*cssSelectorYaml `yaml:"remove_elements"`
	// A css selector for the body of the page. The first element that
	// matches this will be used as the entirety of the page body.
	CssSelectorForBody *cssSelectorYaml `yaml:"css_selector_for_body"`
	// A css selector for the title of the page.
	CssSelectorForTitle *cssSelectorYaml `yaml:"css_selector_for_title"`
	// A 32x32 pixel PNG image.
	Icon32x32 string `yaml:"icon32x32"`
	AllowJS   bool   `yaml:"allowJS"`
	// External URL for "Open Online Page"
	ExternalURL string `yaml:"externalURL"`
}

func (d *Dashing) shouldIgnoreFile(src string) bool {
	// Skip our own config file.
	if filepath.Base(src) == "dashing.yaml" {
		return true
	}

	for _, regex := range d.IgnorePathRegexes {
		if regex.MatchString(src) {
			return true
		}
	}

	// Skip VCS dirs.
	parts := strings.Split(src, "/")
	for _, p := range parts {
		switch p {
		case ".git", ".svn":
			return true
		}
	}
	return false
}

// regexpYaml is a custom type that embeds *regexp.Regexp and implements UnmarshalYAML.
type regexpYaml struct {
	*regexp.Regexp
}

// UnmarshalYAML implements the yaml.Unmarshaler interface for regexpYaml.
func (r *regexpYaml) UnmarshalYAML(value *yaml.Node) error {
	pattern := value.Value
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("invalid regexp pattern '%s': %w", pattern, err)
	}
	r.Regexp = compiled
	return nil
}

type cssSelectorYaml struct {
	cascadia.Sel
}

func (c *cssSelectorYaml) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("expected scalar node for CSS selector, got %v", value.Kind)
	}
	selector, err := css.Parse(value.Value)
	if err != nil {
		return fmt.Errorf("invalid CSS selector '%s': %w", value.Value, err)
	}
	c.Sel = selector
	return nil
}

func (c *cssSelectorYaml) MarshalYAML() (interface{}, error) {
	// Marshal the CSS selector back to its string representation
	if c.Sel == nil {
		return "", fmt.Errorf("nil CSS selector cannot be marshaled")
	}
	return c.Sel.String(), nil
	// Note: We don't need to handle errors here since UnmarshalYAML handles them.
}

// Transform is a description of what should be done with a selector.
type Transform struct {
	CssSelector                cssSelectorYaml  `yaml:"css"`                                      // The CSS selector to match elements
	Type                       string           `yaml:"type"`                                     // The type of the element
	Attribute                  string           `yaml:"attr,omitempty"`                           // Use the value of this attribute as basis
	RequireText                *regexpYaml      `yaml:"requiretext,omitempty"`                    // Require text matches the given regexp
	SkipText                   *regexpYaml      `yaml:"skiptext,omitempty"`                       // Skip this entry if the text matches this regexp
	MatchPath                  *regexpYaml      `yaml:"matchpath,omitempty"`                      // Skip files that don't match this path
	TOCRoot                    bool             `yaml:"toc_root,omitempty"`                       // If true, this entry is a root in the TOC
	TOCChild                   bool             `yaml:"toc_child,omitempty"`                      // If true, this entry is a child in the TOC
	CssSelectorForSearchPrefix *cssSelectorYaml `yaml:"css_selector_for_search_prefix,omitempty"` // If true, use the page title as a prefix in search results
}

func main() {
	app := cli.NewApp()
	app.Name = "dashing"
	app.Usage = "Generate Dash documentation from HTML files"
	app.Version = version

	app.Commands = commands()

	app.Run(os.Args)
}

func commands() []*cli.Command {
	return []*cli.Command{
		{
			Name:   "build",
			Usage:  "build a doc set",
			Action: build,
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:  "config, f",
					Usage: "The path to the YAML configuration file.",
				},
				&cli.StringFlag{
					Name:  "output, o",
					Usage: "The output directory for the docset.",
				},
			},
		},
	}
}

func build(c *cli.Context) error {
	var dashing Dashing

	cf := strings.TrimSpace(c.String("config"))
	if len(cf) == 0 {
		cf = "./dashing.yaml"
	}

	conf, err := ioutil.ReadFile(cf)
	if err != nil {
		fmt.Printf("Failed to open configuration file '%s': %s (Run `dashing init`?)\n", cf, err)
		os.Exit(1)
	}

	if err := yaml.Unmarshal(conf, &dashing); err != nil {
		fmt.Printf("Failed to parse YAML: %s", err)
		os.Exit(1)
	}

	name := dashing.Package

	fmt.Printf("Building %s from files in '%s'.\n", name, dashing.WalkRoot)

	writer := newFileWriter(c.String("output"))

	addPlist(name, &dashing, writer)
	if len(dashing.Icon32x32) > 0 {
		err := writer.copyFile(path.Join(dashing.Icon32x32), "icon.png")
		if err != nil {
			fmt.Printf("Error copying icon: %s\n", err)
		}
	}
	db, err := writer.initDB(name)
	if err != nil {
		fmt.Printf("Failed to create database: %s\n", err)
		return nil
	}
	defer db.Close()
	texasRanger(dashing.WalkRoot, writer, dashing, db)

	// for _, dir := range dashing.CopyDirsIntoDocs {
	// 	fileSystem := os.DirFS(dir)
	// 	fmt.Printf("Copying %s into docset\n", dir)
	// 	// copyFS doesn't like existing files - clear it out
	// 	os.RemoveAll(path.Join(destination, dir))
	// 	err := os.CopyFS(path.Join(destination, dir), fileSystem)
	// 	if err != nil {
	// 		fmt.Printf("Error copying %s: %s\n", dir, err)
	// 	}
	// }

	return nil
}

func addPlist(name string, config *Dashing, writer fileWriter) {
	var file bytes.Buffer
	t := template.Must(template.New("plist").Parse(plist))

	fancyName := config.Name
	if len(fancyName) == 0 {
		fancyName = strings.ToTitle(name)
	}

	aj := "false"
	if config.AllowJS {
		aj = "true"
	}

	tvars := map[string]string{
		"Name":        name,
		"FancyName":   fancyName,
		"Index":       config.Index,
		"AllowJS":     aj,
		"ExternalURL": config.ExternalURL,
	}

	err := t.Execute(&file, tvars)
	if err != nil {
		fmt.Printf("Failed: %s\n", err)
		return
	}
	writer.WriteFile("Contents/Info.plist", file.Bytes(), 0755)
}

// texasRanger is... wait for it... a WALKER!
func texasRanger(base string, writer fileWriter, dashing Dashing, db *sql.DB) error {
	filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() || dashing.shouldIgnoreFile(path) {
			return nil
		}
		if htmlish(path) {
			fmt.Printf("%s looks like HTML\n", path)
			result, err := parseHTML(path, dashing)
			if err != nil {
				fmt.Printf("Error parsing %s: %s\n", path, err)
				return nil
			}
			for _, ref := range result.refs {
				// the real path needs to be:
				// <dash_entry_name=NAME><dash_entry_originalName=BUNDLE_NAME.NAME><dash_entry_menuDescription=BUNDLE_NAME>HTML_FILE#//dash_ref_NAME/TYPE/NAME/LEVEL>
				path := fmt.Sprintf("<dash_entry_name=%s><dash_entry_originalName=%s><dash_entry_menuDescription=%s>%s", ref.name, ref.name, ref.menuDescription, ref.href)
				fmt.Printf("Match(%s): '%s' is type %s at %s\n", ref.selector, ref.name, ref.etype, ref.href)
				fmt.Println(path)
				db.Exec(`INSERT OR IGNORE INTO searchIndex(name, type, path) VALUES (?,?,?)`, ref.name, ref.etype, path)
			}
			writer.addHtml(path, result.htmlNode)
			// for _, file := range result.usedFiles {
			// 	if dashing.shouldIgnoreFile(file) {
			// 		continue
			// 	}
			// 	err = copyFile(file, filepath.Join(destination, file))
			// 	if err != nil {
			// 		fmt.Printf("Error copying %s: %s\n", file, err)
			// 	}
			// }
			return nil
		} else {
			if dashing.shouldIgnoreFile(path) {
				return nil
			}
			err = writer.addContentFile(path)
			// err = copyFile(path, filepath.Join(destination, path))
			if err != nil {
				fmt.Printf("Error copying %s: %s\n", path, err)
			}
		}
		return nil
	})
	return nil
}

func newFileWriter(destRoot string) fileWriter {
	os.MkdirAll(path.Join(destRoot, "Contents/Resources"), 0755)
	return fileWriter{destRoot: destRoot}
}

type fileWriter struct {
	destRoot string
}

func (w fileWriter) WriteFile(filename string, data []byte, perm os.FileMode) error {
	return os.WriteFile(filepath.Join(w.destRoot, filename), data, perm)
}

func (w fileWriter) initDB(name string) (*sql.DB, error) {
	dbname := path.Join(w.destRoot, "Contents/Resources/docSet.dsidx")

	db, err := sql.Open("sqlite3", dbname)
	if err != nil {
		return db, err
	}

	if _, err := db.Exec(`CREATE TABLE searchIndex(id INTEGER PRIMARY KEY, name TEXT, type TEXT, path TEXT)`); err != nil {
		return db, err
	}

	if _, err := db.Exec(`CREATE UNIQUE INDEX anchor ON searchIndex (name, type, path)`); err != nil {
		return db, err
	}

	return db, nil
}

func (w fileWriter) addContentFile(src string) error {
	return copyFile(src, filepath.Join(w.destRoot, "Contents/Resources/Documents", src))
}

func (w fileWriter) copyFile(src string, dest string) error {
	return copyFile(src, filepath.Join(w.destRoot, dest))
}

func (w fileWriter) addHtml(src string, n *html.Node) error {
	return writeHTML(src, filepath.Join(w.destRoot, "Contents/Resources/Documents"), n)
}

func encodeHTMLentities(orig string) string {
	escaped := new(bytes.Buffer)
	for _, c := range orig {
		if point_to_entity[c] == "" {
			escaped.WriteRune(c)
		} else {
			escaped.WriteString(point_to_entity[c])
		}
	}

	return escaped.String()
}

func writeHTML(orig, dest string, root *html.Node) error {
	dir := filepath.Dir(orig)
	base := filepath.Base(orig)
	os.MkdirAll(filepath.Join(dest, dir), 0755)
	out, err := os.Create(filepath.Join(dest, dir, base))
	if err != nil {
		return err
	}
	defer out.Close()

	content_bytes := new(bytes.Buffer)
	html.Render(content_bytes, root)
	content := encodeHTMLentities(content_bytes.String())

	_, err = out.WriteString(content)
	return err
}

func htmlish(filename string) bool {
	e := strings.ToLower(filepath.Ext(filename))
	switch e {
	case ".html", ".htm", ".xhtml", ".html5":
		return true
	}
	return false
}

type reference struct {
	selector, name, etype, href, menuDescription string
}

type parseResult struct {
	refs      []*reference
	usedFiles []string
	htmlNode  *html.Node
}

func parseHTML(filepath string, dashing Dashing) (parseResult, error) {
	r, err := os.Open(filepath)
	if err != nil {
		return parseResult{}, err
	}
	defer r.Close()
	top, err := html.Parse(r)
	if err != nil {
		return parseResult{}, err
	}

	// head
	headMatcher := css.MustCompile("head")
	headNode := headMatcher.MatchFirst(top)

	root := css.MustCompile("*[href],*[src]")
	roots := root.MatchAll(top)
	usedFiles := make([]string, 0)
	for _, node := range roots {
		for _, attribute := range node.Attr {
			if attribute.Key == "href" || attribute.Key == "src" {
				url, err := url.Parse(attribute.Val)
				if err != nil {
					fmt.Printf("ERROR: %s: Error parsing URL '%s': %s\n", filepath, attribute.Val, err)
					continue
				}
				if url.Scheme == "" && url.Host == "" && url.Path != "" {
					// relative path
					toCopy := path.Join(path.Dir(filepath), url.Path)
					usedFiles = append(usedFiles, toCopy)
				}
				break
			}
		}
	}

	for _, selector := range dashing.RemoveElements {
		for _, node := range css.Selector(selector.Sel.Match).MatchAll(top) {
			node.Parent.RemoveChild(node)
		}
	}

	if dashing.CssSelectorForBody != nil {
		// head
		bodyMatcher := css.MustCompile("body")
		bodyNode := bodyMatcher.MatchFirst(top)

		bodyNodeContent := css.Selector(dashing.CssSelectorForBody.Sel.Match).MatchFirst(top)

		if bodyNodeContent == nil {
			fmt.Printf("ERROR: No body found matching '%s' in %s\n", dashing.CssSelectorForBody.String(), filepath)
		} else {
			bodyNodeContent.Parent.RemoveChild(bodyNodeContent)
			bodyNodeContent.Attr = append(bodyNodeContent.Attr, html.Attribute{Key: "style", Val: "max-width: 100%;"})
			// make a new body node
			newBodyNode := &html.Node{
				Type:     html.ElementNode,
				DataAtom: atom.Body,
				Data:     atom.Body.String(),
				Attr:     bodyNode.Attr,
			}
			newBodyNode.AppendChild(bodyNodeContent)
			bodyNode.Parent.InsertBefore(newBodyNode, bodyNode)
			bodyNode.Parent.RemoveChild(bodyNode)
		}
	}

	refs := findRefs(top, dashing.Selectors, dashing, filepath, headNode)
	if len(refs) == 0 {
		refs = findRefs(top, dashing.BackupSelectors, dashing, filepath, headNode)
	}
	return parseResult{
		refs:      refs,
		usedFiles: usedFiles,
		htmlNode:  top,
	}, nil
}

func findRefs(top *html.Node, selectors []Transform, dashing Dashing, filepath string, headNode *html.Node) []*reference {
	refs := []*reference{}
	// tocHeaderName := ""

	titleString := ""
	if dashing.CssSelectorForTitle != nil {
		title := css.Selector(dashing.CssSelectorForTitle.Sel.Match).MatchFirst(top)
		if title != nil {
			titleString = text(title)
		}
	}

	matchingSelectors := make([]*Transform, 0)
	for _, sel := range selectors {
		// Skip this selector if file path doesn't match
		if sel.MatchPath == nil {
			matchingSelectors = append(matchingSelectors, &sel)
		} else {
			if sel.MatchPath.MatchString(filepath) {
				matchingSelectors = append(matchingSelectors, &sel)
			}
		}
	}

	for n := range top.Descendants() {
		for _, sel := range matchingSelectors {
			if !sel.CssSelector.Match(n) {
				continue
			}

			textString := text(n)
			if sel.RequireText != nil && !sel.RequireText.MatchString(textString) {
				fmt.Printf("Skipping entry for '%s' (Text not matching given regexp '%v')\n", textString, sel.RequireText)
				continue
			}

			if sel.SkipText != nil && sel.SkipText.MatchString(textString) {
				fmt.Printf("Skipping entry for '%s' (Text matches skip regexp '%v')\n", textString, sel.SkipText)
				continue
			}

			var name string
			if len(sel.Attribute) != 0 {
				name = attr(n, sel.Attribute)
			} else {
				name = textString
			}

			prefix := ""
			if sel.CssSelectorForSearchPrefix != nil {
				node := css.Query(top, sel.CssSelectorForSearchPrefix)
				if node != nil {
					nodeText := text(node)
					// Only set prefix if it's different from the current name
					// This saves us from having a prefix like "prefix.prefix.name"
					if nodeText != textString {
						prefix = fmt.Sprintf("%s.", nodeText)
					}
				}
			}

			if !strings.HasSuffix(filepath, "-2.html") {
				linkHref := attr(n, "href")
				if !strings.HasPrefix(linkHref, "#") || len(linkHref) == 0 {
					linkHref = "#" + attr(n, "id")
				}
				// if len(linkHref) == 0 and {
				// linkHref = fmt.Sprintf(":~:text=%s", url.QueryEscape(name))
				// linkHref = anchor(n)
				// }
				refs = append(refs,
					&reference{
						selector:        sel.CssSelector.String(),
						name:            prefix + name,
						etype:           sel.Type,
						href:            filepath + linkHref,
						menuDescription: titleString,
					},
				)
			}

			// if sel.TOCRoot {
			// 	tocHeaderName = name
			// }

			tocAnchor, linkNode := tocAnchorAndLinkNode(name, sel.Type, sel.TOCRoot)
			headNode.AppendChild(linkNode)
			n.Parent.InsertBefore(tocAnchor, n)
		}
	}
	return refs
}

func text(node *html.Node) string {
	var b bytes.Buffer
	for c := node.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		} else if c.Type == html.ElementNode {
			b.WriteString(text(c))
		}
	}
	return strings.TrimSpace(b.String())
}

func attr(node *html.Node, key string) string {
	for _, a := range node.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// tcounter is used to generate automatic anchors.
// NOTE: NOT THREADSAFE. If we switch to goroutines, swith to atom int.
var tcounter int

func anchor(node *html.Node) string {
	if node.Type == html.ElementNode && node.Data == "a" {
		for _, a := range node.Attr {
			if a.Key == "name" {
				return a.Val
			}
		}
	}
	tname := fmt.Sprintf("autolink-%d", tcounter)
	link := autolink(tname)
	node.Parent.InsertBefore(link, node)
	tcounter++
	return tname
}

// autolink creates an A tag for when one is not present in original docs.
func autolink(target string) *html.Node {
	return &html.Node{
		Type:     html.ElementNode,
		DataAtom: atom.A,
		Data:     atom.A.String(),
		Attr: []html.Attribute{
			{Key: "class", Val: "dashingAutolink"},
			{Key: "name", Val: target},
		},
	}
}

var counter = 0

func tocAnchorAndLinkNode(name, etype string, isSectionHeader bool) (*html.Node, *html.Node) {
	name = strings.Replace(url.QueryEscape(name), "+", "%20", -1)

	tocLevel := 0 // default level for children
	if isSectionHeader {
		tocLevel = 1 // root level
	}
	target := fmt.Sprintf("//dash_ref_%d/%s/%s/%d", counter, etype, name, tocLevel)
	counter++

	return &html.Node{
			Type:     html.ElementNode,
			DataAtom: atom.A,
			Data:     atom.A.String(),
			Attr: []html.Attribute{
				{Key: "class", Val: "dashAnchor"},
				{Key: "name", Val: target},
			},
		}, &html.Node{
			Type:     html.ElementNode,
			DataAtom: atom.Link,
			Data:     atom.Link.String(),
			Attr: []html.Attribute{
				{Key: "href", Val: target},
			},
		}
}

// copyFile copies a source file to a new destination.
func copyFile(src, dest string) error {
	// if dest exists already, no-op
	if _, err := os.Stat(dest); err == nil {
		return nil
	}

	fmt.Printf("Copying %s\n to %s\n", src, dest)

	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

var point_to_entity = map[rune]string{
	8704: "&forall;",
	8194: "&ensp;",
	8195: "&emsp;",
	8709: "&empty;",
	8711: "&nabla;",
	8712: "&isin;",
	8201: "&thinsp;",
	8715: "&ni;",
	8204: "&zwnj;",
	8205: "&zwj;",
	8206: "&lrm;",
	8719: "&prod;",
	8721: "&sum;",
	8722: "&minus;",
	8211: "&ndash;",
	8212: "&mdash;",
	8727: "&lowast;",
	8216: "&lsquo;",
	8217: "&rsquo;",
	8730: "&radic;",
	175:  "&macr;",
	8220: "&ldquo;",
	8221: "&rdquo;",
	8222: "&bdquo;",
	8224: "&dagger;",
	8225: "&Dagger;",
	8226: "&bull;",
	8230: "&hellip;",
	8743: "&and;",
	8744: "&or;",
	8745: "&cap;",
	8746: "&cup;",
	8747: "&int;",
	8240: "&permil;",
	8242: "&prime;",
	8243: "&Prime;",
	8756: "&there4;",
	8713: "&notin;",
	8249: "&lsaquo;",
	8250: "&rsaquo;",
	8764: "&sim;",
	8629: "&crarr;",
	9824: "&spades;",
	8260: "&frasl;",
	8773: "&cong;",
	8776: "&asymp;",
	8207: "&rlm;",
	9829: "&hearts;",
	8800: "&ne;",
	8801: "&equiv;",
	9827: "&clubs;",
	8804: "&le;",
	8805: "&ge;",
	9830: "&diams;",
	8834: "&sub;",
	8835: "&sup;",
	8836: "&nsub;",
	8838: "&sube;",
	8839: "&supe;",
	8853: "&oplus;",
	8855: "&otimes;",
	8734: "&infin;",
	8218: "&sbquo;",
	8901: "&sdot;",
	160:  "&nbsp;",
	161:  "&iexcl;",
	162:  "&cent;",
	163:  "&pound;",
	164:  "&curren;",
	8869: "&perp;",
	166:  "&brvbar;",
	167:  "&sect;",
	168:  "&uml;",
	169:  "&copy;",
	170:  "&ordf;",
	171:  "&laquo;",
	8364: "&euro;",
	173:  "&shy;",
	174:  "&reg;",
	8733: "&prop;",
	176:  "&deg;",
	177:  "&plusmn;",
	178:  "&sup2;",
	179:  "&sup3;",
	180:  "&acute;",
	181:  "&micro;",
	182:  "&para;",
	183:  "&middot;",
	184:  "&cedil;",
	185:  "&sup1;",
	186:  "&ordm;",
	187:  "&raquo;",
	188:  "&frac14;",
	189:  "&frac12;",
	190:  "&frac34;",
	191:  "&iquest;",
	192:  "&Agrave;",
	193:  "&Aacute;",
	194:  "&Acirc;",
	195:  "&Atilde;",
	196:  "&Auml;",
	197:  "&Aring;",
	198:  "&AElig;",
	199:  "&Ccedil;",
	200:  "&Egrave;",
	201:  "&Eacute;",
	202:  "&Ecirc;",
	203:  "&Euml;",
	204:  "&Igrave;",
	205:  "&Iacute;",
	206:  "&Icirc;",
	207:  "&Iuml;",
	208:  "&ETH;",
	209:  "&Ntilde;",
	210:  "&Ograve;",
	211:  "&Oacute;",
	212:  "&Ocirc;",
	213:  "&Otilde;",
	214:  "&Ouml;",
	215:  "&times;",
	216:  "&Oslash;",
	217:  "&Ugrave;",
	218:  "&Uacute;",
	219:  "&Ucirc;",
	220:  "&Uuml;",
	221:  "&Yacute;",
	222:  "&THORN;",
	223:  "&szlig;",
	224:  "&agrave;",
	225:  "&aacute;",
	226:  "&acirc;",
	227:  "&atilde;",
	228:  "&auml;",
	229:  "&aring;",
	230:  "&aelig;",
	231:  "&ccedil;",
	232:  "&egrave;",
	234:  "&ecirc;",
	235:  "&euml;",
	236:  "&igrave;",
	8658: "&rArr;",
	238:  "&icirc;",
	239:  "&iuml;",
	240:  "&eth;",
	241:  "&ntilde;",
	242:  "&ograve;",
	243:  "&oacute;",
	244:  "&ocirc;",
	245:  "&otilde;",
	246:  "&ouml;",
	247:  "&divide;",
	248:  "&oslash;",
	249:  "&ugrave;",
	250:  "&uacute;",
	251:  "&ucirc;",
	252:  "&uuml;",
	// '/':  "%252F", // Escape forward slash for URLs in hrefs
	253:  "&yacute;",
	254:  "&thorn;",
	255:  "&yuml;",
	172:  "&not;",
	8968: "&lceil;",
	8969: "&rceil;",
	8970: "&lfloor;",
	8971: "&rfloor;",
	8465: "&image;",
	8472: "&weierp;",
	8476: "&real;",
	8482: "&trade;",
	732:  "&tilde;",
	9002: "&rang;",
	8736: "&ang;",
	402:  "&fnof;",
	8706: "&part;",
	8501: "&alefsym;",
	710:  "&circ;",
	338:  "&OElig;",
	339:  "&oelig;",
	352:  "&Scaron;",
	353:  "&scaron;",
	8593: "&uarr;",
	8594: "&rarr;",
	8707: "&exist;",
	8595: "&darr;",
	8254: "&oline;",
	233:  "&eacute;",
	376:  "&Yuml;",
	916:  "&Delta;",
	237:  "&iacute;",
	8592: "&larr;",
	913:  "&Alpha;",
	914:  "&Beta;",
	915:  "&Gamma;",
	8596: "&harr;",
	917:  "&Epsilon;",
	918:  "&Zeta;",
	919:  "&Eta;",
	920:  "&Theta;",
	921:  "&Iota;",
	922:  "&Kappa;",
	923:  "&Lambda;",
	924:  "&Mu;",
	925:  "&Nu;",
	926:  "&Xi;",
	927:  "&Omicron;",
	928:  "&Pi;",
	929:  "&Rho;",
	931:  "&Sigma;",
	932:  "&Tau;",
	933:  "&Upsilon;",
	934:  "&Phi;",
	935:  "&Chi;",
	936:  "&Psi;",
	937:  "&Omega;",
	945:  "&alpha;",
	946:  "&beta;",
	947:  "&gamma;",
	948:  "&delta;",
	949:  "&epsilon;",
	950:  "&zeta;",
	951:  "&eta;",
	952:  "&theta;",
	953:  "&iota;",
	954:  "&kappa;",
	955:  "&lambda;",
	956:  "&mu;",
	957:  "&nu;",
	958:  "&xi;",
	959:  "&omicron;",
	960:  "&pi;",
	961:  "&rho;",
	962:  "&sigmaf;",
	963:  "&sigma;",
	964:  "&tau;",
	965:  "&upsilon;",
	966:  "&phi;",
	967:  "&chi;",
	968:  "&psi;",
	969:  "&omega;",
	9674: "&loz;",
	8656: "&lArr;",
	977:  "&thetasym;",
	978:  "&upsih;",
	8659: "&dArr;",
	8660: "&hArr;",
	982:  "&piv;",
	165:  "&yen;",
	8657: "&uArr;",
	9001: "&lang;",
}
