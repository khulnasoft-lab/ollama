package cmd

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/console"
	"github.com/mattn/go-runewidth"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/auth"
	"github.com/ollama/ollama/envconfig"
	"github.com/ollama/ollama/format"
	"github.com/ollama/ollama/parser"
	"github.com/ollama/ollama/progress"
	"github.com/ollama/ollama/server"
	"github.com/ollama/ollama/types/errtypes"
	"github.com/ollama/ollama/types/model"
	"github.com/ollama/ollama/version"
)

func CreateHandler(cmd *cobra.Command, args []string) error {
	filename, _ := cmd.Flags().GetString("file")
	filename, err := filepath.Abs(filename)
	if err != nil {
		return err
	}

	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}

	p := progress.NewProgress(os.Stderr)
	defer p.Stop()

	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	modelfile, err := parser.ParseFile(f)
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	status := "transferring model data"
	spinner := progress.NewSpinner(status)
	p.Add(status, spinner)
	defer p.Stop()

	for i := range modelfile.Commands {
		switch modelfile.Commands[i].Name {
		case "model", "adapter":
			path := modelfile.Commands[i].Args
			if path == "~" {
				path = home
			} else if strings.HasPrefix(path, "~/") {
				path = filepath.Join(home, path[2:])
			}

			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(filename), path)
			}

			fi, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) && modelfile.Commands[i].Name == "model" {
				continue
			} else if err != nil {
				return err
			}

			if fi.IsDir() {
				// this is likely a safetensors or pytorch directory
				// TODO make this work w/ adapters
				tempfile, err := tempZipFiles(path)
				if err != nil {
					return err
				}
				defer os.RemoveAll(tempfile)

				path = tempfile
			}

			// spinner.Stop()
			digest, err := createBlob(cmd, client, path, spinner)
			if err != nil {
				return err
			}
			modelfile.Commands[i].Args = "@" + digest
		}
	}

	bars := make(map[string]*progress.Bar)
	fn := func(resp api.ProgressResponse) error {
		if resp.Digest != "" {
			spinner.Stop()

			bar, ok := bars[resp.Digest]
			if !ok {
				bar = progress.NewBar(fmt.Sprintf("pulling %s...", resp.Digest[7:19]), resp.Total, resp.Completed)
				bars[resp.Digest] = bar
				p.Add(resp.Digest, bar)
			}

			bar.Set(resp.Completed)
		} else if status != resp.Status {
			spinner.Stop()

			status = resp.Status
			spinner := progress.NewSpinner(status)
			p.Add(status, spinner)
		}

		return nil
	}

	quantize, _ := cmd.Flags().GetString("quantize")

	request := api.CreateRequest{Name: args[0], Modelfile: modelfile.String(), Quantize: quantize}
	if err := client.Create(cmd.Context(), &request, fn); err != nil {
		return err
	}

	return nil
}

func tempZipFiles(path string) (string, error) {
	tempfile, err := os.CreateTemp("", "ollama-tf")
	if err != nil {
		return "", err
	}
	defer tempfile.Close()

	detectContentType := func(path string) (string, error) {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()

		var b bytes.Buffer
		b.Grow(512)

		if _, err := io.CopyN(&b, f, 512); err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}

		contentType, _, _ := strings.Cut(http.DetectContentType(b.Bytes()), ";")
		return contentType, nil
	}

	glob := func(pattern, contentType string) ([]string, error) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}

		for _, safetensor := range matches {
			if ct, err := detectContentType(safetensor); err != nil {
				return nil, err
			} else if ct != contentType {
				return nil, fmt.Errorf("invalid content type: expected %s for %s", ct, safetensor)
			}
		}

		return matches, nil
	}

	var files []string
	if st, _ := glob(filepath.Join(path, "model*.safetensors"), "application/octet-stream"); len(st) > 0 {
		// safetensors files might be unresolved git lfs references; skip if they are
		// covers model-x-of-y.safetensors, model.fp32-x-of-y.safetensors, model.safetensors
		files = append(files, st...)
	} else if pt, _ := glob(filepath.Join(path, "pytorch_model*.bin"), "application/zip"); len(pt) > 0 {
		// pytorch files might also be unresolved git lfs references; skip if they are
		// covers pytorch_model-x-of-y.bin, pytorch_model.fp32-x-of-y.bin, pytorch_model.bin
		files = append(files, pt...)
	} else if pt, _ := glob(filepath.Join(path, "consolidated*.pth"), "application/zip"); len(pt) > 0 {
		// pytorch files might also be unresolved git lfs references; skip if they are
		// covers consolidated.x.pth, consolidated.pth
		files = append(files, pt...)
	} else {
		return "", errors.New("no safetensors or torch files found")
	}

	// add configuration files, json files are detected as text/plain
	js, err := glob(filepath.Join(path, "*.json"), "text/plain")
	if err != nil {
		return "", err
	}
	files = append(files, js...)

	if tks, _ := glob(filepath.Join(path, "tokenizer.model"), "application/octet-stream"); len(tks) > 0 {
		// add tokenizer.model if it exists, tokenizer.json is automatically picked up by the previous glob
		// tokenizer.model might be a unresolved git lfs reference; error if it is
		files = append(files, tks...)
	} else if tks, _ := glob(filepath.Join(path, "**/tokenizer.model"), "text/plain"); len(tks) > 0 {
		// some times tokenizer.model is in a subdirectory (e.g. meta-llama/Meta-Llama-3-8B)
		files = append(files, tks...)
	}

	zipfile := zip.NewWriter(tempfile)
	defer zipfile.Close()

	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			return "", err
		}
		defer f.Close()

		fi, err := f.Stat()
		if err != nil {
			return "", err
		}

		zfi, err := zip.FileInfoHeader(fi)
		if err != nil {
			return "", err
		}

		zf, err := zipfile.CreateHeader(zfi)
		if err != nil {
			return "", err
		}

		if _, err := io.Copy(zf, f); err != nil {
			return "", err
		}
	}

	return tempfile.Name(), nil
}

var ErrBlobExists = errors.New("blob exists")

func createBlob(cmd *cobra.Command, client *api.Client, path string, spinner *progress.Spinner) (string, error) {
	bin, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer bin.Close()

	// Get file info to retrieve the size
	fileInfo, err := bin.Stat()
	if err != nil {
		return "", err
	}
	fileSize := fileInfo.Size()

	hash := sha256.New()
	if _, err := io.Copy(hash, bin); err != nil {
		return "", err
	}

	if _, err := bin.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	var pw progressWriter
	// Create a progress bar and start a goroutine to update it
	// JK Let's use a percentage

	//bar := progress.NewBar("transferring model data...", fileSize, 0)
	//p.Add("transferring model data", bar)

	status := "transferring model data 0%"
	spinner.SetMessage(status)

	ticker := time.NewTicker(60 * time.Millisecond)
	done := make(chan struct{})
	defer close(done)

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				spinner.SetMessage(fmt.Sprintf("transferring model data %d%%", int(100*pw.n/fileSize)))
			case <-done:
				spinner.SetMessage("transferring model data 100%")
				return
			}
		}
	}()

	digest := fmt.Sprintf("sha256:%x", hash.Sum(nil))

	// We check if we can find the models directory locally
	// If we can, we return the path to the directory
	// If we can't, we return an error
	// If the blob exists already, we return the digest
	dest, err := getLocalPath(cmd.Context(), digest)

	if errors.Is(err, ErrBlobExists) {
		return digest, nil
	}

	// Successfully found the model directory
	if err == nil {
		// Copy blob in via OS specific copy
		// Linux errors out to use io.copy
		err = localCopy(path, dest)
		if err == nil {
			return digest, nil
		}

		// Default copy using io.copy
		err = defaultCopy(path, dest)
		if err == nil {
			return digest, nil
		}
	}

	// If at any point copying the blob over locally fails, we default to the copy through the server
	if err = client.CreateBlob(cmd.Context(), digest, io.TeeReader(bin, &pw)); err != nil {
		return "", err
	}
	return digest, nil
}

type progressWriter struct {
	n int64
}

func (w *progressWriter) Write(p []byte) (n int, err error) {
	w.n += int64(len(p))
	return len(p), nil
}

func getLocalPath(ctx context.Context, digest string) (string, error) {
	ollamaHost := envconfig.Host

	client := http.DefaultClient
	base := &url.URL{
		Scheme: ollamaHost.Scheme,
		Host:   net.JoinHostPort(ollamaHost.Host, ollamaHost.Port),
	}

	data, err := json.Marshal(digest)
	if err != nil {
		return "", err
	}

	reqBody := bytes.NewReader(data)
	path := fmt.Sprintf("/api/blobs/%s", digest)
	requestURL := base.JoinPath(path)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), reqBody)
	if err != nil {
		return "", err
	}

	authz, err := api.Authorization(ctx, request)
	if err != nil {
		return "", err
	}

	request.Header.Set("Authorization", authz)
	request.Header.Set("User-Agent", fmt.Sprintf("ollama/%s (%s %s) Go/%s", version.Version, runtime.GOARCH, runtime.GOOS, runtime.Version()))
	request.Header.Set("X-Redirect-Create", "1")

	resp, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTemporaryRedirect {
		dest := resp.Header.Get("LocalLocation")

		return dest, nil
	}
	return "", ErrBlobExists
}

func defaultCopy(path string, dest string) error {
	// This function should be called if the server is local
	// It should find the model directory, copy the blob over, and return the digest
	dirPath := filepath.Dir(dest)

	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		return err
	}

	// Copy blob over
	sourceFile, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("could not open source file: %v", err)
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("could not create destination file: %v", err)
	}
	defer destFile.Close()

	_, err = io.CopyBuffer(destFile, sourceFile, make([]byte, 4*1024*1024))
	if err != nil {
		return fmt.Errorf("error copying file: %v", err)
	}

	err = destFile.Sync()
	if err != nil {
		return fmt.Errorf("error flushing file: %v", err)
	}

	return nil
}

func RunHandler(cmd *cobra.Command, args []string) error {
	interactive := true

	opts := runOptions{
		Model:    args[0],
		WordWrap: os.Getenv("TERM") == "xterm-256color",
		Options:  map[string]interface{}{},
	}

	format, err := cmd.Flags().GetString("format")
	if err != nil {
		return err
	}
	opts.Format = format

	keepAlive, err := cmd.Flags().GetString("keepalive")
	if err != nil {
		return err
	}
	if keepAlive != "" {
		d, err := time.ParseDuration(keepAlive)
		if err != nil {
			return err
		}
		opts.KeepAlive = &api.Duration{Duration: d}
	}

	prompts := args[1:]
	// prepend stdin to the prompt if provided
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		in, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}

		prompts = append([]string{string(in)}, prompts...)
		opts.WordWrap = false
		interactive = false
	}
	opts.Prompt = strings.Join(prompts, " ")
	if len(prompts) > 0 {
		interactive = false
	}

	nowrap, err := cmd.Flags().GetBool("nowordwrap")
	if err != nil {
		return err
	}
	opts.WordWrap = !nowrap

	// Fill out the rest of the options based on information about the
	// model.
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}

	name := args[0]
	info, err := func() (*api.ShowResponse, error) {
		showReq := &api.ShowRequest{Name: name}
		info, err := client.Show(cmd.Context(), showReq)
		var se api.StatusError
		if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
			if err := PullHandler(cmd, []string{name}); err != nil {
				return nil, err
			}
			return client.Show(cmd.Context(), &api.ShowRequest{Name: name})
		}
		return info, err
	}()
	if err != nil {
		return err
	}

	opts.MultiModal = slices.Contains(info.Details.Families, "clip")
	opts.ParentModel = info.Details.ParentModel
	opts.Messages = append(opts.Messages, info.Messages...)

	if interactive {
		return generateInteractive(cmd, opts)
	}
	return generate(cmd, opts)
}

func errFromUnknownKey(unknownKeyErr error) error {
	// find SSH public key in the error message
	sshKeyPattern := `ssh-\w+ [^\s"]+`
	re := regexp.MustCompile(sshKeyPattern)
	matches := re.FindStringSubmatch(unknownKeyErr.Error())

	if len(matches) > 0 {
		serverPubKey := matches[0]

		publicKey, err := auth.GetPublicKey()
		if err != nil {
			return unknownKeyErr
		}

		localPubKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey)))

		if runtime.GOOS == "linux" && serverPubKey != localPubKey {
			// try the ollama service public key
			svcPubKey, err := os.ReadFile("/usr/share/ollama/.ollama/id_ed25519.pub")
			if err != nil {
				return unknownKeyErr
			}
			localPubKey = strings.TrimSpace(string(svcPubKey))
		}

		// check if the returned public key matches the local public key, this prevents adding a remote key to the user's account
		if serverPubKey != localPubKey {
			return unknownKeyErr
		}

		var msg strings.Builder
		msg.WriteString(unknownKeyErr.Error())
		msg.WriteString("\n\nYour ollama key is:\n")
		msg.WriteString(localPubKey)
		msg.WriteString("\nAdd your key at:\n")
		msg.WriteString("https://ollama.com/settings/keys")

		return errors.New(msg.String())
	}

	return unknownKeyErr
}

func PushHandler(cmd *cobra.Command, args []string) error {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}

	insecure, err := cmd.Flags().GetBool("insecure")
	if err != nil {
		return err
	}

	p := progress.NewProgress(os.Stderr)
	defer p.Stop()

	bars := make(map[string]*progress.Bar)
	var status string
	var spinner *progress.Spinner

	fn := func(resp api.ProgressResponse) error {
		if resp.Digest != "" {
			if spinner != nil {
				spinner.Stop()
			}

			bar, ok := bars[resp.Digest]
			if !ok {
				bar = progress.NewBar(fmt.Sprintf("pushing %s...", resp.Digest[7:19]), resp.Total, resp.Completed)
				bars[resp.Digest] = bar
				p.Add(resp.Digest, bar)
			}

			bar.Set(resp.Completed)
		} else if status != resp.Status {
			if spinner != nil {
				spinner.Stop()
			}

			status = resp.Status
			spinner = progress.NewSpinner(status)
			p.Add(status, spinner)
		}

		return nil
	}

	request := api.PushRequest{Name: args[0], Insecure: insecure}
	if err := client.Push(cmd.Context(), &request, fn); err != nil {
		if spinner != nil {
			spinner.Stop()
		}
		if strings.Contains(err.Error(), "access denied") {
			return errors.New("you are not authorized to push to this namespace, create the model under a namespace you own")
		}
		host := model.ParseName(args[0]).Host
		isOllamaHost := strings.HasSuffix(host, ".ollama.ai") || strings.HasSuffix(host, ".ollama.com")
		if strings.Contains(err.Error(), errtypes.UnknownOllamaKeyErrMsg) && isOllamaHost {
			// the user has not added their ollama key to ollama.com
			// re-throw an error with a more user-friendly message
			return errFromUnknownKey(err)
		}

		return err
	}

	spinner.Stop()
	return nil
}

func ListHandler(cmd *cobra.Command, args []string) error {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}

	models, err := client.List(cmd.Context())
	if err != nil {
		return err
	}

	var data [][]string

	for _, m := range models.Models {
		if len(args) == 0 || strings.HasPrefix(m.Name, args[0]) {
			data = append(data, []string{m.Name, m.Digest[:12], format.HumanBytes(m.Size), format.HumanTime(m.ModifiedAt, "Never")})
		}
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"NAME", "ID", "SIZE", "MODIFIED"})
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetHeaderLine(false)
	table.SetBorder(false)
	table.SetNoWhiteSpace(true)
	table.SetTablePadding("\t")
	table.AppendBulk(data)
	table.Render()

	return nil
}

func ListRunningHandler(cmd *cobra.Command, args []string) error {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}

	models, err := client.ListRunning(cmd.Context())
	if err != nil {
		return err
	}

	var data [][]string

	for _, m := range models.Models {
		if len(args) == 0 || strings.HasPrefix(m.Name, args[0]) {
			var procStr string
			switch {
			case m.SizeVRAM == 0:
				procStr = "100% CPU"
			case m.SizeVRAM == m.Size:
				procStr = "100% GPU"
			case m.SizeVRAM > m.Size || m.Size == 0:
				procStr = "Unknown"
			default:
				sizeCPU := m.Size - m.SizeVRAM
				cpuPercent := math.Round(float64(sizeCPU) / float64(m.Size) * 100)
				procStr = fmt.Sprintf("%d%%/%d%% CPU/GPU", int(cpuPercent), int(100-cpuPercent))
			}
			data = append(data, []string{m.Name, m.Digest[:12], format.HumanBytes(m.Size), procStr, format.HumanTime(m.ExpiresAt, "Never")})
		}
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"NAME", "ID", "SIZE", "PROCESSOR", "UNTIL"})
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetHeaderLine(false)
	table.SetBorder(false)
	table.SetNoWhiteSpace(true)
	table.SetTablePadding("\t")
	table.AppendBulk(data)
	table.Render()

	return nil
}

func DeleteHandler(cmd *cobra.Command, args []string) error {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}

	for _, name := range args {
		req := api.DeleteRequest{Name: name}
		if err := client.Delete(cmd.Context(), &req); err != nil {
			return err
		}
		fmt.Printf("deleted '%s'\n", name)
	}
	return nil
}

func ShowHandler(cmd *cobra.Command, args []string) error {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}

	license, errLicense := cmd.Flags().GetBool("license")
	modelfile, errModelfile := cmd.Flags().GetBool("modelfile")
	parameters, errParams := cmd.Flags().GetBool("parameters")
	system, errSystem := cmd.Flags().GetBool("system")
	template, errTemplate := cmd.Flags().GetBool("template")

	for _, boolErr := range []error{errLicense, errModelfile, errParams, errSystem, errTemplate} {
		if boolErr != nil {
			return errors.New("error retrieving flags")
		}
	}

	flagsSet := 0
	showType := ""

	if license {
		flagsSet++
		showType = "license"
	}

	if modelfile {
		flagsSet++
		showType = "modelfile"
	}

	if parameters {
		flagsSet++
		showType = "parameters"
	}

	if system {
		flagsSet++
		showType = "system"
	}

	if template {
		flagsSet++
		showType = "template"
	}

	if flagsSet > 1 {
		return errors.New("only one of '--license', '--modelfile', '--parameters', '--system', or '--template' can be specified")
	}

	req := api.ShowRequest{Name: args[0]}
	resp, err := client.Show(cmd.Context(), &req)
	if err != nil {
		return err
	}

	if flagsSet == 1 {
		switch showType {
		case "license":
			fmt.Println(resp.License)
		case "modelfile":
			fmt.Println(resp.Modelfile)
		case "parameters":
			fmt.Println(resp.Parameters)
		case "system":
			fmt.Println(resp.System)
		case "template":
			fmt.Println(resp.Template)
		}

		return nil
	}

	showInfo(resp)

	return nil
}

func showInfo(resp *api.ShowResponse) {
	arch := resp.ModelInfo["general.architecture"].(string)

	modelData := [][]string{
		{"arch", arch},
		{"parameters", resp.Details.ParameterSize},
		{"quantization", resp.Details.QuantizationLevel},
		{"context length", fmt.Sprintf("%v", resp.ModelInfo[fmt.Sprintf("%s.context_length", arch)].(float64))},
		{"embedding length", fmt.Sprintf("%v", resp.ModelInfo[fmt.Sprintf("%s.embedding_length", arch)].(float64))},
	}

	mainTableData := [][]string{
		{"Model"},
		{renderSubTable(modelData, false)},
	}

	if resp.ProjectorInfo != nil {
		projectorData := [][]string{
			{"arch", "clip"},
			{"parameters", format.HumanNumber(uint64(resp.ProjectorInfo["general.parameter_count"].(float64)))},
		}

		if projectorType, ok := resp.ProjectorInfo["clip.projector_type"]; ok {
			projectorData = append(projectorData, []string{"projector type", projectorType.(string)})
		}

		projectorData = append(projectorData,
			[]string{"embedding length", fmt.Sprintf("%v", resp.ProjectorInfo["clip.vision.embedding_length"].(float64))},
			[]string{"projection dimensionality", fmt.Sprintf("%v", resp.ProjectorInfo["clip.vision.projection_dim"].(float64))},
		)

		mainTableData = append(mainTableData,
			[]string{"Projector"},
			[]string{renderSubTable(projectorData, false)},
		)
	}

	if resp.Parameters != "" {
		mainTableData = append(mainTableData, []string{"Parameters"}, []string{formatParams(resp.Parameters)})
	}

	if resp.System != "" {
		mainTableData = append(mainTableData, []string{"System"}, []string{renderSubTable(twoLines(resp.System), true)})
	}

	if resp.License != "" {
		mainTableData = append(mainTableData, []string{"License"}, []string{renderSubTable(twoLines(resp.License), true)})
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetAutoWrapText(false)
	table.SetBorder(false)
	table.SetAlignment(tablewriter.ALIGN_LEFT)

	for _, v := range mainTableData {
		table.Append(v)
	}

	table.Render()
}

func renderSubTable(data [][]string, file bool) string {
	var buf bytes.Buffer
	table := tablewriter.NewWriter(&buf)
	table.SetAutoWrapText(!file)
	table.SetBorder(false)
	table.SetNoWhiteSpace(true)
	table.SetTablePadding("\t")
	table.SetAlignment(tablewriter.ALIGN_LEFT)

	for _, v := range data {
		table.Append(v)
	}

	table.Render()

	renderedTable := buf.String()
	lines := strings.Split(renderedTable, "\n")
	for i, line := range lines {
		lines[i] = "\t" + line
	}

	return strings.Join(lines, "\n")
}

func twoLines(s string) [][]string {
	lines := strings.Split(s, "\n")
	res := [][]string{}

	count := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			count++
			res = append(res, []string{line})
			if count == 2 {
				return res
			}
		}
	}
	return res
}

func formatParams(s string) string {
	lines := strings.Split(s, "\n")
	table := [][]string{}

	for _, line := range lines {
		table = append(table, strings.Fields(line))
	}
	return renderSubTable(table, false)
}

func CopyHandler(cmd *cobra.Command, args []string) error {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}

	req := api.CopyRequest{Source: args[0], Destination: args[1]}
	if err := client.Copy(cmd.Context(), &req); err != nil {
		return err
	}
	fmt.Printf("copied '%s' to '%s'\n", args[0], args[1])
	return nil
}

func PullHandler(cmd *cobra.Command, args []string) error {
	insecure, err := cmd.Flags().GetBool("insecure")
	if err != nil {
		return err
	}

	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}

	p := progress.NewProgress(os.Stderr)
	defer p.Stop()

	bars := make(map[string]*progress.Bar)

	var status string
	var spinner *progress.Spinner

	fn := func(resp api.ProgressResponse) error {
		if resp.Digest != "" {
			if spinner != nil {
				spinner.Stop()
			}

			bar, ok := bars[resp.Digest]
			if !ok {
				bar = progress.NewBar(fmt.Sprintf("pulling %s...", resp.Digest[7:19]), resp.Total, resp.Completed)
				bars[resp.Digest] = bar
				p.Add(resp.Digest, bar)
			}

			bar.Set(resp.Completed)
		} else if status != resp.Status {
			if spinner != nil {
				spinner.Stop()
			}

			status = resp.Status
			spinner = progress.NewSpinner(status)
			p.Add(status, spinner)
		}

		return nil
	}

	request := api.PullRequest{Name: args[0], Insecure: insecure}
	if err := client.Pull(cmd.Context(), &request, fn); err != nil {
		return err
	}

	return nil
}

type generateContextKey string

type runOptions struct {
	Model       string
	ParentModel string
	Prompt      string
	Messages    []api.Message
	WordWrap    bool
	Format      string
	System      string
	Template    string
	Images      []api.ImageData
	Options     map[string]interface{}
	MultiModal  bool
	KeepAlive   *api.Duration
}

type displayResponseState struct {
	lineLength int
	wordBuffer string
}

func displayResponse(content string, wordWrap bool, state *displayResponseState) {
	termWidth, _, _ := term.GetSize(int(os.Stdout.Fd()))
	if wordWrap && termWidth >= 10 {
		for _, ch := range content {
			if state.lineLength+1 > termWidth-5 {
				if runewidth.StringWidth(state.wordBuffer) > termWidth-10 {
					fmt.Printf("%s%c", state.wordBuffer, ch)
					state.wordBuffer = ""
					state.lineLength = 0
					continue
				}

				// backtrack the length of the last word and clear to the end of the line
				a := runewidth.StringWidth(state.wordBuffer)
				if a > 0 {
					fmt.Printf("\x1b[%dD", a)
				}
				fmt.Printf("\x1b[K\n")
				fmt.Printf("%s%c", state.wordBuffer, ch)
				chWidth := runewidth.RuneWidth(ch)

				state.lineLength = runewidth.StringWidth(state.wordBuffer) + chWidth
			} else {
				fmt.Print(string(ch))
				state.lineLength += runewidth.RuneWidth(ch)
				if runewidth.RuneWidth(ch) >= 2 {
					state.wordBuffer = ""
					continue
				}

				switch ch {
				case ' ':
					state.wordBuffer = ""
				case '\n':
					state.lineLength = 0
				default:
					state.wordBuffer += string(ch)
				}
			}
		}
	} else {
		fmt.Printf("%s%s", state.wordBuffer, content)
		if len(state.wordBuffer) > 0 {
			state.wordBuffer = ""
		}
	}
}

func chat(cmd *cobra.Command, opts runOptions) (*api.Message, error) {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return nil, err
	}

	p := progress.NewProgress(os.Stderr)
	defer p.StopAndClear()

	spinner := progress.NewSpinner("")
	p.Add("", spinner)

	cancelCtx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)

	go func() {
		<-sigChan
		cancel()
	}()

	var state *displayResponseState = &displayResponseState{}
	var latest api.ChatResponse
	var fullResponse strings.Builder
	var role string

	fn := func(response api.ChatResponse) error {
		p.StopAndClear()

		latest = response

		role = response.Message.Role
		content := response.Message.Content
		fullResponse.WriteString(content)

		displayResponse(content, opts.WordWrap, state)

		return nil
	}

	req := &api.ChatRequest{
		Model:    opts.Model,
		Messages: opts.Messages,
		Format:   opts.Format,
		Options:  opts.Options,
	}

	if opts.KeepAlive != nil {
		req.KeepAlive = opts.KeepAlive
	}

	if err := client.Chat(cancelCtx, req, fn); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, nil
		}
		return nil, err
	}

	if len(opts.Messages) > 0 {
		fmt.Println()
		fmt.Println()
	}

	verbose, err := cmd.Flags().GetBool("verbose")
	if err != nil {
		return nil, err
	}

	if verbose {
		latest.Summary()
	}

	return &api.Message{Role: role, Content: fullResponse.String()}, nil
}

func generate(cmd *cobra.Command, opts runOptions) error {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}

	p := progress.NewProgress(os.Stderr)
	defer p.StopAndClear()

	spinner := progress.NewSpinner("")
	p.Add("", spinner)

	var latest api.GenerateResponse

	generateContext, ok := cmd.Context().Value(generateContextKey("context")).([]int)
	if !ok {
		generateContext = []int{}
	}

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT)

	go func() {
		<-sigChan
		cancel()
	}()

	var state *displayResponseState = &displayResponseState{}

	fn := func(response api.GenerateResponse) error {
		p.StopAndClear()

		latest = response
		content := response.Response

		displayResponse(content, opts.WordWrap, state)

		return nil
	}

	if opts.MultiModal {
		opts.Prompt, opts.Images, err = extractFileData(opts.Prompt)
		if err != nil {
			return err
		}
	}

	request := api.GenerateRequest{
		Model:     opts.Model,
		Prompt:    opts.Prompt,
		Context:   generateContext,
		Images:    opts.Images,
		Format:    opts.Format,
		System:    opts.System,
		Template:  opts.Template,
		Options:   opts.Options,
		KeepAlive: opts.KeepAlive,
	}

	if err := client.Generate(ctx, &request, fn); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}

	if opts.Prompt != "" {
		fmt.Println()
		fmt.Println()
	}

	if !latest.Done {
		return nil
	}

	verbose, err := cmd.Flags().GetBool("verbose")
	if err != nil {
		return err
	}

	if verbose {
		latest.Summary()
	}

	ctx = context.WithValue(cmd.Context(), generateContextKey("context"), latest.Context)
	cmd.SetContext(ctx)

	return nil
}

func RunServer(cmd *cobra.Command, _ []string) error {
	if err := initializeKeypair(); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(envconfig.Host.Host, envconfig.Host.Port))
	if err != nil {
		return err
	}

	err = server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}

func initializeKeypair() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	privKeyPath := filepath.Join(home, ".ollama", "id_ed25519")
	pubKeyPath := filepath.Join(home, ".ollama", "id_ed25519.pub")

	_, err = os.Stat(privKeyPath)
	if os.IsNotExist(err) {
		fmt.Printf("Couldn't find '%s'. Generating new private key.\n", privKeyPath)
		cryptoPublicKey, cryptoPrivateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}

		privateKeyBytes, err := ssh.MarshalPrivateKey(cryptoPrivateKey, "")
		if err != nil {
			return err
		}

		if err := os.MkdirAll(filepath.Dir(privKeyPath), 0o755); err != nil {
			return fmt.Errorf("could not create directory %w", err)
		}

		if err := os.WriteFile(privKeyPath, pem.EncodeToMemory(privateKeyBytes), 0o600); err != nil {
			return err
		}

		sshPublicKey, err := ssh.NewPublicKey(cryptoPublicKey)
		if err != nil {
			return err
		}

		publicKeyBytes := ssh.MarshalAuthorizedKey(sshPublicKey)

		if err := os.WriteFile(pubKeyPath, publicKeyBytes, 0o644); err != nil {
			return err
		}

		fmt.Printf("Your new public key is: \n\n%s\n", publicKeyBytes)
	}
	return nil
}

func checkServerHeartbeat(cmd *cobra.Command, _ []string) error {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}
	if err := client.Heartbeat(cmd.Context()); err != nil {
		if !strings.Contains(err.Error(), " refused") {
			return err
		}
		if err := startApp(cmd.Context(), client); err != nil {
			return fmt.Errorf("could not connect to ollama app, is it running?")
		}
	}
	return nil
}

func versionHandler(cmd *cobra.Command, _ []string) {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return
	}

	serverVersion, err := client.Version(cmd.Context())
	if err != nil {
		fmt.Println("Warning: could not connect to a running Ollama instance")
	}

	if serverVersion != "" {
		fmt.Printf("ollama version is %s\n", serverVersion)
	}

	if serverVersion != version.Version {
		fmt.Printf("Warning: client version is %s\n", version.Version)
	}
}

func appendEnvDocs(cmd *cobra.Command, envs []envconfig.EnvVar) {
	if len(envs) == 0 {
		return
	}

	envUsage := `
Environment Variables:
`
	for _, e := range envs {
		envUsage += fmt.Sprintf("      %-24s   %s\n", e.Name, e.Description)
	}

	cmd.SetUsageTemplate(cmd.UsageTemplate() + envUsage)
}

func NewCLI() *cobra.Command {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	cobra.EnableCommandSorting = false

	if runtime.GOOS == "windows" {
		console.ConsoleFromFile(os.Stdin) //nolint:errcheck
	}

	rootCmd := &cobra.Command{
		Use:           "ollama",
		Short:         "Large language model runner",
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		Run: func(cmd *cobra.Command, args []string) {
			if version, _ := cmd.Flags().GetBool("version"); version {
				versionHandler(cmd, args)
				return
			}

			cmd.Print(cmd.UsageString())
		},
	}

	rootCmd.Flags().BoolP("version", "v", false, "Show version information")

	createCmd := &cobra.Command{
		Use:     "create MODEL",
		Short:   "Create a model from a Modelfile",
		Args:    cobra.ExactArgs(1),
		PreRunE: checkServerHeartbeat,
		RunE:    CreateHandler,
	}

	createCmd.Flags().StringP("file", "f", "Modelfile", "Name of the Modelfile")
	createCmd.Flags().StringP("quantize", "q", "", "Quantize model to this level (e.g. q4_0)")

	showCmd := &cobra.Command{
		Use:     "show MODEL",
		Short:   "Show information for a model",
		Args:    cobra.ExactArgs(1),
		PreRunE: checkServerHeartbeat,
		RunE:    ShowHandler,
	}

	showCmd.Flags().Bool("license", false, "Show license of a model")
	showCmd.Flags().Bool("modelfile", false, "Show Modelfile of a model")
	showCmd.Flags().Bool("parameters", false, "Show parameters of a model")
	showCmd.Flags().Bool("template", false, "Show template of a model")
	showCmd.Flags().Bool("system", false, "Show system message of a model")

	runCmd := &cobra.Command{
		Use:     "run MODEL [PROMPT]",
		Short:   "Run a model",
		Args:    cobra.MinimumNArgs(1),
		PreRunE: checkServerHeartbeat,
		RunE:    RunHandler,
	}

	runCmd.Flags().String("keepalive", "", "Duration to keep a model loaded (e.g. 5m)")
	runCmd.Flags().Bool("verbose", false, "Show timings for response")
	runCmd.Flags().Bool("insecure", false, "Use an insecure registry")
	runCmd.Flags().Bool("nowordwrap", false, "Don't wrap words to the next line automatically")
	runCmd.Flags().String("format", "", "Response format (e.g. json)")
	serveCmd := &cobra.Command{
		Use:     "serve",
		Aliases: []string{"start"},
		Short:   "Start ollama",
		Args:    cobra.ExactArgs(0),
		RunE:    RunServer,
	}

	pullCmd := &cobra.Command{
		Use:     "pull MODEL",
		Short:   "Pull a model from a registry",
		Args:    cobra.ExactArgs(1),
		PreRunE: checkServerHeartbeat,
		RunE:    PullHandler,
	}

	pullCmd.Flags().Bool("insecure", false, "Use an insecure registry")

	pushCmd := &cobra.Command{
		Use:     "push MODEL",
		Short:   "Push a model to a registry",
		Args:    cobra.ExactArgs(1),
		PreRunE: checkServerHeartbeat,
		RunE:    PushHandler,
	}

	pushCmd.Flags().Bool("insecure", false, "Use an insecure registry")

	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List models",
		PreRunE: checkServerHeartbeat,
		RunE:    ListHandler,
	}

	psCmd := &cobra.Command{
		Use:     "ps",
		Short:   "List running models",
		PreRunE: checkServerHeartbeat,
		RunE:    ListRunningHandler,
	}

	copyCmd := &cobra.Command{
		Use:     "cp SOURCE DESTINATION",
		Short:   "Copy a model",
		Args:    cobra.ExactArgs(2),
		PreRunE: checkServerHeartbeat,
		RunE:    CopyHandler,
	}

	deleteCmd := &cobra.Command{
		Use:     "rm MODEL [MODEL...]",
		Short:   "Remove a model",
		Args:    cobra.MinimumNArgs(1),
		PreRunE: checkServerHeartbeat,
		RunE:    DeleteHandler,
	}

	envVars := envconfig.AsMap()

	envs := []envconfig.EnvVar{envVars["OLLAMA_HOST"]}

	for _, cmd := range []*cobra.Command{
		createCmd,
		showCmd,
		runCmd,
		pullCmd,
		pushCmd,
		listCmd,
		psCmd,
		copyCmd,
		deleteCmd,
		serveCmd,
	} {
		switch cmd {
		case runCmd:
			appendEnvDocs(cmd, []envconfig.EnvVar{envVars["OLLAMA_HOST"], envVars["OLLAMA_NOHISTORY"]})
		case serveCmd:
			appendEnvDocs(cmd, []envconfig.EnvVar{
				envVars["OLLAMA_DEBUG"],
				envVars["OLLAMA_HOST"],
				envVars["OLLAMA_KEEP_ALIVE"],
				envVars["OLLAMA_MAX_LOADED_MODELS"],
				envVars["OLLAMA_MAX_QUEUE"],
				envVars["OLLAMA_MODELS"],
				envVars["OLLAMA_NUM_PARALLEL"],
				envVars["OLLAMA_NOPRUNE"],
				envVars["OLLAMA_ORIGINS"],
				envVars["OLLAMA_TMPDIR"],
				envVars["OLLAMA_FLASH_ATTENTION"],
				envVars["OLLAMA_LLM_LIBRARY"],
				envVars["OLLAMA_MAX_VRAM"],
			})
		default:
			appendEnvDocs(cmd, envs)
		}
	}

	rootCmd.AddCommand(
		serveCmd,
		createCmd,
		showCmd,
		runCmd,
		pullCmd,
		pushCmd,
		listCmd,
		psCmd,
		copyCmd,
		deleteCmd,
	)

	return rootCmd
}
