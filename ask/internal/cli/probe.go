//go:build readonlyprobe

package cli

import "github.com/spf13/cobra"

// init adds the hidden --probe-readonly flag, which skips the read-only
// gate so an unverified tool can be tested by hand.
func init() {
	registerProbe = func(cmd *cobra.Command, o *options) {
		f := cmd.Flags()
		f.BoolVar(&o.probeReadOnly, "probe-readonly", false, "skip the read-only gate (probe build only)")
		if err := f.MarkHidden("probe-readonly"); err != nil {
			panic(err)
		}
	}
}
