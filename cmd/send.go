package cmd

import (
	"fmt"
	"time"

	"github.com/claudiodangelis/qrcp/config"
	"github.com/claudiodangelis/qrcp/logger"
	"github.com/claudiodangelis/qrcp/manifest"
	"github.com/claudiodangelis/qrcp/qr"
	"github.com/claudiodangelis/qrcp/server"
	"github.com/eiannone/keyboard"
	"github.com/spf13/cobra"
)

const mib = 1024 * 1024

func sendCmdFunc(command *cobra.Command, args []string) error {
	log := logger.New(app.Flags.Quiet)
	cfg := config.New(app)
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = 4
	}
	if cfg.TTL == "" {
		cfg.TTL = "24h"
	}
	ttl, err := time.ParseDuration(cfg.TTL)
	if err != nil {
		return fmt.Errorf("invalid --ttl value %q: %w", cfg.TTL, err)
	}
	m, err := manifest.Build(args, int64(cfg.ChunkSize)*mib)
	if err != nil {
		return err
	}
	files, dirs, totalSize := 0, 0, int64(0)
	for _, e := range m.Files {
		switch e.Type {
		case manifest.EntryFile:
			files++
			totalSize += e.Size
		case manifest.EntryDir:
			dirs++
		}
	}
	session, err := manifest.NewSession(ttl)
	if err != nil {
		return err
	}
	srv, err := server.New(&cfg)
	if err != nil {
		return err
	}
	// Sets the manifest session served over QCTP before accepting connections,
	// so requests can never observe an unconfigured server.
	srv.Send(m, session)
	srv.Serve()
	log.Print(fmt.Sprintf(
		"已准备 %d 个文件（%d 个目录，共 %s），分块 %d MiB，会话有效期至 %s",
		files, dirs, humanBytes(totalSize), cfg.ChunkSize,
		session.ExpiresAt.Format("2006-01-02 15:04:05")))
	log.Print(`Scan the following URL with a QR reader to start the file transfer, press CTRL+C or "q" to exit:`)
	log.Print(srv.SendURL)
	qr.RenderString(srv.SendURL, cfg.Reversed)
	if app.Flags.Browser {
		srv.DisplayQR(srv.SendURL)
	}
	if err := keyboard.Open(); err == nil {
		defer func() {
			keyboard.Close()
		}()
		go func() {
			for {
				char, key, _ := keyboard.GetKey()
				if string(char) == "q" || key == keyboard.KeyCtrlC {
					srv.Shutdown()
				}
			}
		}()
	} else {
		log.Print(fmt.Sprintf("Warning: keyboard not detected: %v", err))
	}
	return srv.Wait()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

var sendCmd = &cobra.Command{
	Use:     "send",
	Short:   "Send a file(s) or directories from this host",
	Long:    "Send a file(s) or directories from this host over the resumable chunked transfer protocol",
	Aliases: []string{"s"},
	Example: `# Send /path/file.gif. Webserver listens on a random port
qrcp send /path/file.gif
# Send two files in parallel chunks with resume and integrity verification
qrcp send /path/file1.gif /path/file2.gif
# Send the content of a directory (structure is recreated on the receiver)
qrcp send /path/directory
# Send file.gif by creating a webserver on port 8080
qrcp --port 8080 /path/file.gif
# Use 8 MiB chunks and a 2 hour session
qrcp --chunk-size 8 --ttl 2h /path/bigfile.bin
`,
	Args: cobra.MinimumNArgs(1),
	RunE: sendCmdFunc,
}
