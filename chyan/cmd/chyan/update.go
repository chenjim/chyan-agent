// update.go — `chyan update`: check releases and print the matching artifact
// plus its SHA256. Deliberately no download/install step (D33): signature and
// rollback cost outweigh the convenience, so the user stays in control.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"chyan.tech/chyan/internal/update"
)

// defaultReleasesURL is where GitHub-compatible release metadata lives.
const defaultReleasesURL = "https://api.github.com/repos/chenjim/chyan-agent"

// updateOptions configure one update check (tests inject platform + client).
type updateOptions struct {
	releasesURL string
	current     string
	goos        string
	goarch      string
	hc          *http.Client
}

func updateCmd() *cobra.Command {
	var releases string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Check for a newer release (prints download URL + SHA256, never installs)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			base := releases
			if base == "" {
				base = os.Getenv("CHYAN_RELEASES_URL")
			}
			if base == "" {
				base = defaultReleasesURL
			}
			return runUpdate(cmd.Context(), os.Stdout, updateOptions{
				releasesURL: base, current: version,
				goos: runtime.GOOS, goarch: runtime.GOARCH,
				hc: &http.Client{Timeout: 10 * time.Second},
			})
		},
	}
	cmd.Flags().StringVar(&releases, "releases-url", "", "releases API base (or CHYAN_RELEASES_URL)")
	return cmd
}

// runUpdate reports the latest release without touching the local install.
// A network failure is non-fatal: the user gets a short offline path instead
// of a stack trace, and scripts keep working.
func runUpdate(ctx context.Context, out io.Writer, o updateOptions) error {
	checker := update.NewChecker(o.releasesURL, o.current, o.hc)
	info, err := checker.CheckUpdate(ctx)
	if err != nil {
		fmt.Fprintf(out, "检查更新失败：%v\n", err)
		fmt.Fprintf(out, "离线升级：打开 %s 手动下载对应平台包，解压后替换 chyan 可执行文件。\n", o.releasesURL)
		return nil
	}
	if info == nil {
		fmt.Fprintf(out, "已是最新版本（%s）\n", o.current)
		return nil
	}

	rel := info.Latest
	ver := rel.Version()
	archive := update.ArchiveName(ver, o.goos, o.goarch)
	fmt.Fprintf(out, "发现新版本：%s → %s\n", info.Current, rel.TagName)
	if rel.HTMLURL != "" {
		fmt.Fprintf(out, "发布说明：%s\n", rel.HTMLURL)
	}
	fmt.Fprintf(out, "平台产物：%s\n", archive)
	if url := rel.ArchiveURL(archive); url != "" {
		fmt.Fprintf(out, "下载地址：%s\n", url)
	}

	// The checksum lives in the release's SHA256SUMS asset (release.sh writes
	// it). Only an explicitly listed asset is fetched: a derived URL may 404,
	// and a scary network error is worse than "verify manually".
	if sumsURL := rel.AssetURL(update.SumsAsset); sumsURL != "" {
		if contents, ferr := checker.FetchText(ctx, sumsURL); ferr == nil {
			if sum, ok := update.SumFor(contents, archive); ok {
				fmt.Fprintf(out, "SHA256：%s\n", sum)
				fmt.Fprintf(out, "校验：printf '%%s  %s\\n' %s | sha256sum -c -\n", archive, sum)
			} else {
				fmt.Fprintf(out, "SHA256：%s 中未找到 %s，请手动核对。\n", update.SumsAsset, archive)
			}
		} else {
			fmt.Fprintf(out, "SHA256：获取 %s 失败：%v\n", sumsURL, ferr)
		}
	} else {
		fmt.Fprintf(out, "SHA256：发布资产中未找到 %s，请在发布页手动核对。\n", update.SumsAsset)
	}
	fmt.Fprintln(out, "本命令只提示不替换：确认校验值后再手动替换可执行文件。")
	return nil
}
