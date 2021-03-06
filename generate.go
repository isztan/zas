/*
 * Copyright (c) 2013 Dario Castañé.
 * This file is part of Zas.
 *
 * Zas is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Zas is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Zas.  If not, see <http://www.gnu.org/licenses/>.
 */
package main

import (
	"bytes"
	"fmt"
	"github.com/moovweb/gokogiri"
	"github.com/moovweb/gokogiri/html"
	"github.com/moovweb/gokogiri/xml"
	markdown "github.com/russross/blackfriday"
	"github.com/melvinmt/gt"
	thtml "html/template"
	"io"
	"io/ioutil"
	yaml "launchpad.net/goyaml"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	ttext "text/template"
)

var cmdGenerate = &Subcommand{
	UsageLine: "generate",
}

/*
 * Convenience type to group relevant rendering info.
 */
type Generator struct {
	// Config from ZAS_CONF_FILE.
	Config ConfigSection
	// Default layout from Config[ZAS]["layout"].
	Layout *thtml.Template
	// i18n helper.
	I18n *gt.Build
	// ZasDirectoryConfigs cache
	cachedZasDirectoryConfigs map[string]ConfigSection
}

/*
 * Returns deployment base path in config.
 */
func (gen *Generator) GetDeployPath() string {
	return gen.Config.GetZString("deploy")
}

/*
 * Builds deployment path for specific file pointed by path.
 */
func (gen *Generator) BuildDeployPath(path string) string {
	return filepath.Join(gen.GetDeployPath(), path)
}

/*
 * Renders and writes current file "path" with context "data".
 */
func (gen *Generator) Generate(path string, data *ZasData) (err error) {
	writer, err := os.OpenFile(gen.BuildDeployPath(data.Path), os.O_CREATE|os.O_TRUNC|os.O_RDWR, os.FileMode(ZAS_DEFAULT_FILE_PERM))
	if err != nil {
		return
	}
	defer writer.Close()
	return gen.Layout.Execute(writer, data)
}

func init() {
	cmdGenerate.Run = func() {
		var (
			gen Generator
			err error
		)
		if gen.Config, err = NewConfig(); err != nil {
			panic(err)
		}
		if gen.Layout, err = thtml.ParseFiles(gen.Config.GetZString("layout")); err != nil {
			panic(err)
		}
		i18nStrings, _ := NewI18n()
		gen.I18n = &gt.Build {
			Index: i18nStrings,
			Origin: gen.Config.GetSection("site").GetString("language"),
		}
		deployPath := gen.GetDeployPath()
		// If deployment path already exists, it must be deleted.
		if _, err := os.Stat(deployPath); err == nil {
			if err = os.RemoveAll(deployPath); err != nil {
				panic(err)
			}
		}
		if err = os.Mkdir(deployPath, os.FileMode(ZAS_DEFAULT_DIR_PERM)); err != nil {
			panic(err)
		}
		// Walking function. It allows to bubble up any error from generator.
		walk := func(path string, info os.FileInfo, err error) error {
			return gen.walk(path, info, err)
		}
		if err := filepath.Walk(".", walk); err != nil {
			panic(err)
		}
	}
	cmdGenerate.Init()
}

/*
 * Real walking function. Handles all supported files and copy not supported ones in current deployment path.
 */
func (gen *Generator) walk(path string, info os.FileInfo, err error) (ierr error) {
	if strings.HasPrefix(path, ".") {
		return nil
	}
	switch {
	case info.IsDir():
		ierr = os.Mkdir(gen.BuildDeployPath(path), os.FileMode(ZAS_DEFAULT_DIR_PERM))
	case strings.HasSuffix(path, ".md"):
		ierr = gen.renderMarkdown(path)
	case strings.HasSuffix(path, ".html"):
		ierr = gen.renderHTML(path)
	default:
		ierr = gen.copy(gen.BuildDeployPath(path), path)
	}
	return
}

/*
 * Renders a Markdown file.
 */
func (gen *Generator) renderMarkdown(path string) (err error) {
	input, err := ioutil.ReadFile(path)
	if err != nil {
		return
	}
	md := markdown.MarkdownCommon(input)
	return gen.render(path, md)
}

/*
 * Renders a HTML file.
 */
func (gen *Generator) renderHTML(path string) (err error) {
	input, err := ioutil.ReadFile(path)
	if err != nil {
		return
	}
	return gen.render(path, input)
}

/*
 * Loads ZAS_DIR_CONF_FILE (as defined in constants.go) from current
 * directory or previously found ones.
 * It must be a YAML file.
 */
func (gen *Generator) loadZasDirectoryConfig(currentpath string) (config ConfigSection, err error) {
	var ok bool
	path := filepath.Dir(currentpath)
	if config, ok = gen.cachedZasDirectoryConfigs[path]; !ok {
		data, err := ioutil.ReadFile(fmt.Sprintf("%s/%s", path, ZAS_DIR_CONF_FILE))
		if err != nil {
			// Maybe .zas.yml is in an upper directory (already cached or not),
			// so we call this recursively.
			// Unless we are at current working directory.
			if path == "." {
				return nil, err
			}
			return gen.loadZasDirectoryConfig(path)
		}
		config = make(ConfigSection)
		err = yaml.Unmarshal(data, &config)
		if gen.cachedZasDirectoryConfigs == nil {
			gen.cachedZasDirectoryConfigs = make(map[string]ConfigSection)
		}
		gen.cachedZasDirectoryConfigs[path] = config
	}
	return
}

/*
 * Generic render function. It expects input to be a valid HTML document.
 * Input can be a valid Go template.
 */
func (gen *Generator) render(path string, input []byte) (err error) {
	template, err := ttext.New("current").Parse(string(input))
	if err != nil {
		return
	}
	var processed bytes.Buffer
	// Building context and rendering template.
	data := NewZasData(path, gen)
	data.Directory, _ = gen.loadZasDirectoryConfig(path)
	if err = template.Execute(&processed, &data); err != nil {
		return
	}
	// Here we manipulate its result.
	doc, err := gokogiri.ParseHtml(processed.Bytes())
	if err != nil {
		return
	}
	defer doc.Free()
	if err = gen.handleEmbedTags(doc); err != nil {
		return
	}
	gen.cleanUnnecessaryPTags(doc)
	// TODO Add "magical" i18n support (automatically translate
	// text and links started by slash).
	data.Page, err = gen.extractPageConfig(doc)
	if err != nil {
		fmt.Println(path, "=>", err)
		err = nil
	}
	data.FirstTitle = gen.getTitle(doc)
	body, err := doc.Search("//body")
	if err != nil {
		return
	}
	if len(body) > 0 {
		data.Body = thtml.HTML(body[0].InnerHtml())
	}
	return gen.Generate(path, &data)
}

/*
 * Removes unnecessary paragraph HTML tags generated during Markdown processing by
 * deleting any <p> without child text nodes (just to avoid deletion if semantic tags
 * are inside).
 */
func (gen *Generator) cleanUnnecessaryPTags(doc *html.HtmlDocument) (err error) {
	ps, err := doc.Search("//p")
	if err != nil {
		return
	}
	for _, p := range ps {
		hasText := false
		child := p.FirstChild()
		for child != nil {
			typ := child.NodeType()
			if typ == xml.XML_TEXT_NODE {
				// Little heuristic to remove nodes with visually empty content.
				content := strings.TrimSpace(child.Content())
				if content != "" {
					hasText = true
					break
				}
			}
			child = child.NextSibling()
		}
		// If current <p> tag doesn't have any child text node, extract children and add to its parent.
		if !hasText {
			parent := p.Parent()
			child = p.FirstChild()
			for child != nil {
				parent.AddChild(child)
				child = child.NextSibling()
			}
			p.Remove()
		}
	}
	return
}

/*
 * Returns first H1 tag as page title.
 */
func (gen *Generator) getTitle(doc *html.HtmlDocument) (title string) {
	result, _ := doc.Search("//h1")
	if len(result) > 0 {
		title = result[0].FirstChild().Content()
	}
	return
}

/*
 * Extracts first HTML commend as map. It expects it as a valid YAML map.
 */
func (gen *Generator) extractPageConfig(doc *html.HtmlDocument) (config map[interface{}]interface{}, err error) {
	result, _ := doc.Search("//comment()")
	if len(result) > 0 {
		err = yaml.Unmarshal([]byte(result[0].Content()), &config)
	}
	return
}

/*
 * Copies a file.
 */
func (gen *Generator) copy(dstPath string, srcPath string) (err error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, os.FileMode(ZAS_DEFAULT_FILE_PERM))
	if err != nil {
		return
	}
	defer dst.Close()
	_, err = io.Copy(dst, src)
	return
}

/*
 * Embeds a Markdown file.
 */
func (gen *Generator) Markdown(e xml.Node, doc *html.HtmlDocument) (err error) {
	src := e.Attribute("src").Value()
	mdInput, err := ioutil.ReadFile(src)
	if err != nil {
		return err
	}
	md := markdown.MarkdownCommon(mdInput)
	mdDoc, err := gokogiri.ParseHtml(md)
	if err != nil {
		return err
	}
	partial, err := mdDoc.Search("//body")
	if err != nil {
		return err
	}
	parent := e.Parent()
	child := partial[0].FirstChild()
	for child != nil {
		parent.AddChild(child)
		child = child.NextSibling()
	}
	e.Remove()
	return
}

/*
 * Handles <embed> tags.
 *
 * They can be handled with MIME type plugins or internal exported methods like Markdown.
 */
func (gen *Generator) handleEmbedTags(doc *html.HtmlDocument) (err error) {
	result, err := doc.Search("//embed")
	if err != nil {
		return
	}
	for _, e := range result {
		plugin := gen.resolveMIMETypePlugin(e.Attribute("type").Value())
		method := reflect.ValueOf(gen).MethodByName(strings.Title(plugin))
		if method == reflect.ValueOf(nil) {
			err = gen.handleMIMETypePlugin(e, doc)
		} else {
			args := make([]reflect.Value, 2)
			args[0] = reflect.ValueOf(e)
			args[1] = reflect.ValueOf(doc)
			r := method.Call(args)
			rerr := r[0].Interface()
			if ierr, ok := rerr.(error); ok {
				err = ierr
			}
		}
		if err != nil {
			return
		}
	}
	return
}

type bufErr struct {
	buffer []byte
	err    error
}

/*
 * Invokes a MIME type plugin based on current node's type attribute, passing src attribute's value
 * as argument. Subcommand's output is piped to Gokogiri through a buffer.
 */
func (gen *Generator) handleMIMETypePlugin(e xml.Node, doc *html.HtmlDocument) (err error) {
	src := e.Attribute("src").Value()
	typ := e.Attribute("type").Value()
	cmd := exec.Command(fmt.Sprintf("m%s%s", ZAS_PREFIX, gen.resolveMIMETypePlugin(typ)), src)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = os.Stderr
	c := make(chan bufErr)
	go func() {
		data, err := ioutil.ReadAll(stdout)
		c <- bufErr{data, err}
	}()
	if err = cmd.Start(); err != nil {
		return
	}
	be := <-c
	if err = cmd.Wait(); err != nil {
		return
	}
	if be.err != nil {
		return be.err
	}
	parent := e.Parent()
	child, err := doc.Coerce(be.buffer)
	if err != nil {
		return
	}
	parent.AddChild(child)
	e.Remove()
	return
}

/*
 * Returns registered plugin (without ZAS_PREFIX) from config.
 */
func (gen *Generator) resolveMIMETypePlugin(typ string) string {
	return gen.Config.GetSection("mimetypes").GetString(typ)
}
