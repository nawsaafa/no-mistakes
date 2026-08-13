package cli

import (
	"fmt"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/spf13/cobra"
)

func newEjectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "eject",
		Short: "Show gate removal information for the current repository",
		Long: `Git remains the sole custody authority for no-mistakes, so this command
never deletes anything. It reports the gate's on-disk paths and database
record, and prints the manual steps to remove them yourself.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("eject", func() error {
				p, d, err := openResources()
				if err != nil {
					return err
				}
				defer d.Close()

				repo, err := gate.Eject(cmd.Context(), d, p, ".")
				if err != nil {
					return fmt.Errorf("eject: %w", err)
				}

				w := cmd.OutOrStdout()
				fmt.Fprintf(w, "  %s no-mistakes does not remove gates automatically\n", sYellow.Render("!"))
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  repo"), repo.WorkingPath)
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  gate"), p.RepoDir(repo.ID))
				remoteURL := repo.UpstreamURL
				if repo.ForkURL != "" {
					remoteURL = safeurl.Redact(remoteURL)
				}
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("remote"), remoteURL)
				if repo.ForkURL != "" {
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  fork"), safeurl.Redact(repo.ForkURL))
				}
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s\n", sDim.Render("To remove it yourself:"))
				fmt.Fprintf(w, "    git remote remove %s\n", gate.RemoteName)
				fmt.Fprintf(w, "    rm -rf %s\n", p.RepoDir(repo.ID))
				fmt.Fprintf(w, "    rm -rf %s\n", filepath.Join(p.WorktreesDir(), repo.ID))
				return nil
			})
		},
	}
}
