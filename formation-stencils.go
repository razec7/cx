package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/cloud66-oss/cloud66"
	"github.com/cloud66/cli"
	"github.com/fsnotify/fsnotify"
	"github.com/mgutz/ansi"
)

func stencilSubCommands() []cli.Command {
	return []cli.Command{
		cli.Command{
			Name:   "list",
			Usage:  "List all formation stencils",
			Action: runListStencils,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "formation",
					Usage: "Specify the formation to use",
				},
				cli.StringFlag{
					Name:  "stack,s",
					Usage: "full or partial stack name. This can be omitted if the current directory is a stack directory",
				},
				cli.StringFlag{
					Name:  "output,o",
					Usage: "tailor output view (standard|wide)",
				},
			},
			Description: `Fetch all formation stencils and their templates
Examples:
$ cx formations stencils list --formation foo
$ cx formations stencils list --formation bar
`,
		},
		{
			Name:   "show",
			Usage:  "Shows the content of a single stencil",
			Action: runShowStencil,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "formation",
					Usage: "Specify the formation to use",
				},
				cli.StringFlag{
					Name:  "stack,s",
					Usage: "full or partial stack name. This can be omitted if the current directory is a stack directory",
				},
				cli.StringFlag{
					Name:  "stencil",
					Usage: "Stencil filename",
				},
			},
		},
		{
			Name:   "render",
			Usage:  "Renders a stencil based on the given content without committing it into the Formation git repository",
			Action: runRenderStencil,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "formation",
					Usage: "Specify the formation to use",
				},
				cli.StringFlag{
					Name:  "stack,s",
					Usage: "full or partial stack name. This can be omitted if the current directory is a stack directory",
				},
				cli.StringFlag{
					Name:  "stencil-file",
					Usage: "Stencil filename. This can be a full file path but the file name should be identical to the one available as part of the Formation",
				},
				cli.StringFlag{
					Name:  "stencil-folder",
					Usage: "Render all files within the folder. Cannot be used with stencil-file at the same time",
				},
				cli.StringFlag{
					Name:  "snapshot",
					Usage: "Snapshot ID. Default uses the latest snapshot",
				},
				cli.StringFlag{
					Name:  "output",
					Usage: "Full file name and path to save the rendered stencil. If missing it will output to stdout",
				},
				cli.BoolFlag{
					Name:  "watch",
					Usage: "Watches the file or the folder for changes and renders every time there is a new change",
				},
				cli.BoolFlag{
					Name:  "ignore-errors",
					Usage: "if set, it will return anything that can be rendered and ignores the errors",
				},
				cli.BoolFlag{
					Name:  "ignore-warnings",
					Usage: "if set, it will return anything that can be rendered and ignores the warnings",
				},
			},
		},
		{
			Name:   "add",
			Usage:  "Add a stencil to the formation",
			Action: runAddStencil,
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "formation",
					Usage: "Specify the formation to use",
				},
				cli.StringFlag{
					Name:  "stack,s",
					Usage: "Full or partial stack name. This can be omitted if the current directory is a stack directory or there is a .cx.yml file present",
				},
				cli.StringFlag{
					Name:  "stencil",
					Usage: "Stencil file",
				},
				cli.StringFlag{
					Name:  "service",
					Usage: "Service context of the stencil, if applicable",
				},
				cli.StringFlag{
					Name:  "template",
					Usage: "Template filename",
				},
				cli.StringFlag{
					Name:  "base-template",
					Usage: "Base Template Repository UUID",
				},
				cli.IntFlag{
					Name:  "sequence",
					Usage: "Stencil sequence",
				},
				cli.StringFlag{
					Name:  "message",
					Usage: "Commit message",
				},
				cli.StringFlag{
					Name:  "tags",
					Usage: "Comma separated tags",
				},
			},
		},
	}
}
func runListStencils(c *cli.Context) {
	stack := mustStack(c)
	w := tabwriter.NewWriter(os.Stdout, 1, 2, 2, ' ', 0)
	defer w.Flush()

	formationName := getArgument(c, "formation")
	if formationName == "" {
		printFatal("No formation provided. Please use --formation to specify a formation")
	}

	var formations []cloud66.Formation
	var err error
	formations, err = client.Formations(stack.Uid, true)
	must(err)

	output := c.String("output")
	if output == "" {
		output = "standard"
	}

	for _, formation := range formations {
		if formation.Name == formationName {
			printStencils(w, formation, output)
			return
		}
	}

	printFatal("No formation named '%s' found", formationName)
}

func runRenderStencil(c *cli.Context) {
	stack := mustStack(c)

	formationName := getArgument(c, "formation")
	if formationName == "" {
		printFatal("No formation provided. Please use --formation or use .cx.yml to specify a formation")
	}

	stencilFolder := c.String("stencil-folder")
	stencilFilename := c.String("stencil-file")
	if stencilFilename == "" && stencilFolder == "" {
		printFatal("No stencil file or folder provided. Please use --stencil-file or --stencil-folder to specify a stencil file or folder")
	}
	if stencilFolder != "" && stencilFilename != "" {
		printFatal("Both --stencil-file and --stencil-folder provided. Please use only one")
	}

	output := c.String("output")
	snapshotIDParam := getArgument(c, "snapshot")
	stdout := (output == "")
	watch := c.Bool("watch")
	ignoreWarnings := c.Bool("ignore-warnings")
	ignoreErrors := c.Bool("ignore-errors")

	if watch && stdout {
		printFatal("Cannot use --watch without --output")
	}

	filesToRender := make([]string, 0)
	if stencilFolder != "" {
		fileList, err := ioutil.ReadDir(stencilFolder)
		if err != nil {
			printFatal("Failed to fetch all files from folder %s: %s", stencilFolder, err.Error())
		}
		for _, file := range fileList {
			filesToRender = append(filesToRender, filepath.Join(stencilFolder, file.Name()))
		}
	} else {
		filesToRender = append(filesToRender, stencilFilename)
	}

	// find the snapshot
	var snapshotUID string
	if snapshotIDParam == "" || snapshotIDParam == "latest" {
		snapshots, err := client.Snapshots(stack.Uid)
		must(err)
		sort.Sort(snapshotsByDate(snapshots))
		if len(snapshots) == 0 {
			printFatal("No snapshots found")
		}

		snapshotUID = snapshots[0].Uid
	} else {
		snapshotUID = snapshotIDParam
	}

	var formations []cloud66.Formation
	var err error
	var formation *cloud66.Formation
	formations, err = client.Formations(stack.Uid, false)
	must(err)
	for idx, f := range formations {
		if f.Name == formationName {
			formation = &formations[idx]
		}
	}
	if formation == nil {
		printFatal("No formation named '%s' found", formationName)
	}

	var outdir string
	// if output is defined, then make sure we have a folder for it
	if !stdout {
		if stencilFolder != "" {
			outdir = output
		} else {
			outdir = filepath.Dir(output)
		}

		os.MkdirAll(outdir, os.ModePerm)
	}

	if len(filesToRender) == 0 {
		printFatal("No files found to render")
	}

	for _, stencil := range filesToRender {
		file := filepath.Base(stencil)
		if stencilFolder != "" {
			output = filepath.Join(outdir, file)
		}

		// output filename is sequenced if provided. otherwise, it's concatenated
		renderStencil(stencil, formation, stack, output, snapshotUID, ignoreWarnings, ignoreErrors)
	}

	if watch {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			printFatal("Failed to setup a file watcher: %s", err.Error())
		}
		defer watcher.Close()

		done := make(chan bool)

		fmt.Println("Watching for changes...")

		go func() {
			for {
				select {
				case event, ok := <-watcher.Events:
					if !ok {
						return
					}
					if event.Op&fsnotify.Write == fsnotify.Write {
						changedFile := filepath.Base(event.Name)
						output := filepath.Join(outdir, changedFile)
						renderStencil(event.Name, formation, stack, output, snapshotUID, ignoreWarnings, ignoreErrors)
					}
				case err, ok := <-watcher.Errors:
					if !ok {
						return
					}
					printFatal("Error during file change event: %s", err.Error())
				}
			}
		}()

		for _, file := range filesToRender {
			err = watcher.Add(file)
			if err != nil {
				printFatal("Failed to add a watch for %s: %s", file, err.Error())
			}
		}
		<-done
	}
}

func renderStencil(stencilFilename string,
	formation *cloud66.Formation,
	stack *cloud66.Stack,
	output string,
	snapshotUID string,
	ignoreWarnings bool,
	ignoreErrors bool) {

	if does, _ := fileExists(stencilFilename); !does {
		printFatal("Cannot find %s", stencilFilename)
	}
	// find the file. it should exist
	stencilName := filepath.Base(stencilFilename)

	// we can't render inlines for now
	if strings.HasPrefix(stencilName, "_") {
		return
	}

	stencilUID := ""

	for _, stencil := range formation.Stencils {
		if stencil.Filename == stencilName {
			// we have the stencil get the ID
			stencilUID = stencil.Uid
		}
	}
	if stencilUID == "" {
		printFatal("No stencil named '%s' found", stencilName)
	}

	if stencilUID == "" {
		return
	}

	// Read file to byte slice
	body, err := ioutil.ReadFile(stencilFilename)
	if err != nil {
		printFatal("Failed to read %s: %s", stencilFilename, err.Error())
	}

	// check the checksum
	if output != "" {
		checksum := generateChecksum(body)
		readChecksum, err := readMagicComment(output, "checksum")

		if err != nil {
			// ignore the error and carry on
			fmt.Fprintf(os.Stderr, ansi.Color(fmt.Sprintf("Failed to read the checksum: %s\n", err.Error()), "yellow"))
		} else {
			if checksum == readChecksum {
				// they are equal. skip
				fmt.Fprintf(os.Stdout, fmt.Sprintf("No change found in %s\n", output))
				return
			}
		}

		fmt.Printf("[%s] Rendering %s to %s\n", formation.Name, stencilFilename, output)
	}

	var renders *cloud66.Renders
	renders, err = client.RenderStencil(stack.Uid, snapshotUID, formation.Uid, stencilUID, body)
	must(err)

	foundErrors := renders.Errors()
	if len(foundErrors) != 0 {
		fmt.Fprintln(os.Stderr, ansi.Color("Error during rendering of stencils:", "red+h"))
		for _, renderError := range foundErrors {
			fmt.Fprintf(os.Stderr, ansi.Color(fmt.Sprintf("\t%s in %s\n", renderError.Text, renderError.Stencil), "red+h"))
		}

		if !ignoreErrors {
			return
		}
	}

	foundWarnings := renders.Warnings()
	if len(foundWarnings) != 0 {
		fmt.Fprintln(os.Stderr, ansi.Color("Warning during rendering of stencils:", "yellow"))
		for _, renderError := range foundWarnings {
			fmt.Fprintf(os.Stderr, ansi.Color(fmt.Sprintf("\t%s in %s\n", renderError.Text, renderError.Stencil), "yellow"))
		}

		if !ignoreWarnings {
			return
		}
	}

	// content
	for _, v := range renders.Stencils {
		content := v.Content
		// add magic content
		checksum := generateChecksum(body)
		content = fmt.Sprintf("# cx.checksum: %s\n%s", checksum, content)
		// to a file
		if output != "" {
			err = ioutil.WriteFile(output, []byte(content), 0644)
			if err != nil {
				printFatal(err.Error())
			}
		} else {
			// concatenate
			fmt.Printf("%s---\n", content)
		}
	}
}

func runShowStencil(c *cli.Context) {
	stack := mustStack(c)

	formationName := getArgument(c, "formation")
	if formationName == "" {
		printFatal("No formation provided. Please use --formation to specify a formation")
	}

	stencilName := c.String("stencil")
	if stencilName == "" {
		printFatal("No stencil name provided. Please use --stencil to specify a stencil")
	}

	var formations []cloud66.Formation
	var err error
	formations, err = client.Formations(stack.Uid, true)
	must(err)

	foundStencil := false

	for _, formation := range formations {
		if formation.Name == formationName {
			for _, stencil := range formation.Stencils {
				if stencil.Filename == stencilName {
					printStencil(stencil)
					foundStencil = true
				}
			}

			if !foundStencil {
				printFatal("No stencil named '%s' found", stencilName)
			}
			return
		}
	}

	printFatal("No formation named '%s' found", formationName)
}

func runAddStencil(c *cli.Context) {
	stack := mustStack(c)

	formationName := getArgument(c, "formation")
	if formationName == "" {
		printFatal("No formation provided. Please use --formation to specify a formation")
	}

	stencilFile := c.String("stencil")
	if stencilFile == "" {
		printFatal("No stencil filename provided. Please use --stencil to specify a stencil file")
	}

	btrUUID := getArgument(c, "base-template")
	if btrUUID == "" {
		printFatal("No base template uuid provided. Please use --base-template to specify a stencil file")
	}

	tags := []string{}
	contextID := c.String("service")
	template := c.String("template")
	sequence := c.Int("sequence")
	message := c.String("message")
	tagList := c.String("tags")
	if tagList != "" {
		tags = strings.Split(tagList, ",")
	}

	var formations []cloud66.Formation
	var err error
	formations, err = client.Formations(stack.Uid, true)
	must(err)
	var foundFormation cloud66.Formation

	for _, formation := range formations {
		if formation.Name == formationName {
			for _, stencil := range formation.Stencils {
				if stencil.Filename == stencilFile {
					// there is a stencil with the same name. abort
					printFatal("Another stencil with the same name is found. You can use the update command to update it")
					return
				}
			}
			foundFormation = formation
		}
	}

	// if stencilFile doesn't exist, we need to fetch the contents of the template from the BTR and fill in the file with it instead
	if does, _ := fileExists(stencilFile); !does {
		btr, err := client.GetBaseTemplate(btrUUID, true, true)
		if err != nil {
			printFatal(err.Error())
		}

		for _, stencil := range btr.Stencils {
			if stencil.Filename == template {
				err = ioutil.WriteFile(stencilFile, []byte(stencil.Content), 0644)
				if err != nil {
					printFatal(err.Error())
				}
			}
		}
	}

	if err := addStencil(stack, &foundFormation, btrUUID, stencilFile, contextID, template, sequence, message, tags); err != nil {
		printFatal(err.Error())
	}

	fmt.Println("Stencil was added to formation")
}

func printStencils(w io.Writer, formation cloud66.Formation, output string) {
	stencils := formation.Stencils
	sort.Sort(stencilBySequence(stencils))

	if output == "standard" {
		listRec(w,
			"UID",
			"FILENAME",
			"TAGS",
			"CREATED AT",
			"LAST UPDATED")
	} else {
		listRec(w,
			"UID",
			"FILENAME",
			"SERVICE",
			"TAGS",
			"TEMPLATE",
			"GITFILE",
			"INLINE",
			"CREATED AT",
			"LAST UPDATED")
	}

	for _, a := range stencils {
		listStencil(w, a, output)
	}
}

func printStencil(stencil cloud66.Stencil) {
	var buffer bytes.Buffer

	buffer.WriteString(stencil.Body)
	fmt.Print(buffer.String())
}

func addStencil(stack *cloud66.Stack, formation *cloud66.Formation, btrUuid string, stencilFile string, contextID string, templateFilename string, sequence int, message string, tags []string) error {
	body, err := ioutil.ReadFile(stencilFile)
	if err != nil {
		return err
	}

	remoteFilename := filepath.Base(stencilFile)
	stencil := &cloud66.Stencil{
		Filename:         remoteFilename,
		TemplateFilename: templateFilename,
		ContextID:        contextID,
		Tags:             tags,
		Body:             string(body),
		Sequence:         sequence,
	}

	_, err = client.AddStencils(stack.Uid, formation.Uid, btrUuid, []*cloud66.Stencil{stencil}, message)
	if err != nil {
		return err
	}

	return nil
}

func listStencil(w io.Writer, a cloud66.Stencil, output string) {
	ta := a.CreatedAt

	if output == "standard" {
		listRec(w,
			a.Uid,
			a.Filename,
			a.Tags,
			prettyTime{ta},
			prettyTime{a.UpdatedAt},
		)
	} else {
		listRec(w,
			a.Uid,
			a.Filename,
			a.ContextID,
			a.Tags,
			a.TemplateFilename,
			a.GitfilePath,
			a.Inline,
			prettyTime{ta},
			prettyTime{a.UpdatedAt})
	}
}

type stencilBySequence []cloud66.Stencil

func (a stencilBySequence) Len() int           { return len(a) }
func (a stencilBySequence) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a stencilBySequence) Less(i, j int) bool { return a[i].Sequence < a[j].Sequence }
