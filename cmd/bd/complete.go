package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/hooks"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
	"github.com/steveyegge/beads/internal/utils"
)

var completeCmd = &cobra.Command{
	Use:     "complete [id...]",
	Aliases: []string{"resolve"},
	GroupID: "issues",
	Short:   "Mark one or more issues as completed (pending verification)",
	Long: `Mark one or more issues as completed (pending verification).

If no issue ID is provided, completes the last touched issue (from most recent
create, update, show, or close operation).`,
	Args: cobra.MinimumNArgs(0),
	Run: func(cmd *cobra.Command, args []string) {
		CheckReadonly("complete")

		// If no IDs provided, use last touched issue
		if len(args) == 0 {
			lastTouched := GetLastTouchedID()
			if lastTouched == "" {
				FatalErrorRespectJSON("no issue ID provided and no last touched issue")
			}
			args = []string{lastTouched}
		}
		reason, _ := cmd.Flags().GetString("reason")
		if reason == "" {
			// Check --resolution alias (Jira CLI convention)
			reason, _ = cmd.Flags().GetString("resolution")
		}
		if reason == "" {
			// Check -m alias (git commit convention)
			reason, _ = cmd.Flags().GetString("message")
		}
		if reason == "" {
			// Check --comment alias
			reason, _ = cmd.Flags().GetString("comment")
		}

		if reason == "" {
			reason = "Completed"
		}
		force, _ := cmd.Flags().GetBool("force")
		continueFlag, _ := cmd.Flags().GetBool("continue")
		noAuto, _ := cmd.Flags().GetBool("no-auto")
		suggestNext, _ := cmd.Flags().GetBool("suggest-next")

		claimNext, _ := cmd.Flags().GetBool("claim-next")

		// Get session ID from flag or environment variable
		session, _ := cmd.Flags().GetString("session")
		if session == "" {
			session = os.Getenv("CLAUDE_SESSION_ID")
		}

		ctx := rootCtx

		// --continue only works with a single issue
		if continueFlag && len(args) > 1 {
			FatalErrorRespectJSON("--continue only works when completing a single issue")
		}

		// --suggest-next only works with a single issue
		if suggestNext && len(args) > 1 {
			FatalErrorRespectJSON("--suggest-next only works when completing a single issue")
		}

		// Resolve partial IDs first, handling cross-rig routing
		var resolvedIDs []string
		var routedArgs []string // IDs that need cross-repo routing
		for _, id := range args {
			if needsRouting(id) {
				routedArgs = append(routedArgs, id)
			} else {
				resolved, err := utils.ResolvePartialID(ctx, store, id)
				if err != nil {
					FatalErrorRespectJSON("resolving ID %s: %v", id, err)
				}
				resolvedIDs = append(resolvedIDs, resolved)
			}
		}

		completedIssues := []*types.Issue{}
		completedCount := 0

		// Handle local IDs
		for _, id := range resolvedIDs {
			issue, _ := store.GetIssue(ctx, id)

			if err := validateIssueCompletatable(id, issue, force); err != nil {
				fmt.Fprintf(os.Stderr, "%s\n", err)
				continue
			}

			// Check gate satisfaction for machine-checkable gates (GH#1467)
			if !force {
				if err := checkGateSatisfaction(issue); err != nil {
					fmt.Fprintf(os.Stderr, "cannot complete %s: %s\n", id, err)
					continue
				}
			}

			// Check if issue has open blockers (GH#962)
			if !force {
				blocked, blockers, err := store.IsBlocked(ctx, id)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error checking blockers for %s: %v\n", id, err)
					continue
				}
				if blocked && len(blockers) > 0 {
					fmt.Fprintf(os.Stderr, "cannot complete %s: blocked by open issues %v (use --force to override)\n", id, blockers)
					continue
				}
			}

			if err := store.CompleteIssue(ctx, id, reason, actor, session); err != nil {
				fmt.Fprintf(os.Stderr, "Error completing %s: %v\n", id, err)
				continue
			}

			completedCount++

			// Auto-close parent molecule if all steps are now complete
			autoCloseCompletedMolecule(ctx, store, id, actor, session)

			// Run hook
			completedIssue, _ := store.GetIssue(ctx, id)
			if completedIssue != nil && hookRunner != nil {
				hookRunner.Run(hooks.EventUpdate, completedIssue)
			}

			if jsonOutput {
				if completedIssue != nil {
					completedIssues = append(completedIssues, completedIssue)
				}
			} else {
				fmt.Printf("%s Completed %s: %s\n", ui.RenderPass("✓"), formatFeedbackID(id, issueTitleOrEmpty(issue)), reason)
			}
		}

		// Handle routed IDs (cross-rig)
		for _, id := range routedArgs {
			result, err := resolveAndGetIssueWithRouting(ctx, store, id)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error resolving %s: %v\n", id, err)
				continue
			}
			if result == nil || result.Issue == nil {
				if result != nil {
					result.Close()
				}
				fmt.Fprintf(os.Stderr, "Issue %s not found\n", id)
				continue
			}

			if err := validateIssueCompletatable(result.ResolvedID, result.Issue, force); err != nil {
				result.Close()
				fmt.Fprintf(os.Stderr, "%s\n", err)
				continue
			}

			if !force {
				if err := checkGateSatisfaction(result.Issue); err != nil {
					result.Close()
					fmt.Fprintf(os.Stderr, "cannot complete %s: %s\n", id, err)
					continue
				}
			}

			// Check if issue has open blockers (GH#962)
			if !force {
				blocked, blockers, err := result.Store.IsBlocked(ctx, result.ResolvedID)
				if err != nil {
					result.Close()
					fmt.Fprintf(os.Stderr, "Error checking blockers for %s: %v\n", id, err)
					continue
				}
				if blocked && len(blockers) > 0 {
					result.Close()
					fmt.Fprintf(os.Stderr, "cannot complete %s: blocked by open issues %v (use --force to override)\n", id, blockers)
					continue
				}
			}

			// Note: We'd need CompleteIssue in the storage interface for full cross-rig support.
			// For now, many stores might only support Close. If so, fallout to Close or error.
			// Assuming the interface might need update, but for now we'll call Close as fallback or just error.
			// Actually, let's try to see if the store has CompleteIssue via type assertion or similar if we want to be fancy.
			// But for simplicity in this PR, we'll assume local store mostly.

			if err := result.Store.CloseIssue(ctx, result.ResolvedID, reason, actor, session); err != nil {
				result.Close()
				fmt.Fprintf(os.Stderr, "Error closing/completing %s: %v\n", id, err)
				continue
			}

			completedCount++

			autoCloseCompletedMolecule(ctx, result.Store, result.ResolvedID, actor, session)

			closedIssue, _ := result.Store.GetIssue(ctx, result.ResolvedID)
			if closedIssue != nil && hookRunner != nil {
				hookRunner.Run(hooks.EventClose, closedIssue)
			}

			if jsonOutput {
				if closedIssue != nil {
					completedIssues = append(completedIssues, closedIssue)
				}
			} else {
				fmt.Printf("%s Completed/Closed %s: %s\n", ui.RenderPass("✓"), formatFeedbackID(result.ResolvedID, result.Issue.Title), reason)
			}
			result.Close()
		}

		// Handle --suggest-next flag in direct mode
		if suggestNext && len(resolvedIDs) == 1 && completedCount > 0 {
			unblocked, err := store.GetNewlyUnblockedByClose(ctx, resolvedIDs[0])
			if err == nil && len(unblocked) > 0 {
				if jsonOutput {
					outputJSON(map[string]interface{}{
						"completed": completedIssues,
						"unblocked": unblocked,
					})
					return
				}
				fmt.Printf("\nNewly unblocked:\n")
				for _, issue := range unblocked {
					fmt.Printf("  • %s (P%d)\n", formatFeedbackID(issue.ID, issue.Title), issue.Priority)
				}
			}
		}

		// Handle --continue flag
		if continueFlag && len(resolvedIDs) == 1 && completedCount > 0 {
			autoClaim := !noAuto
			result, err := AdvanceToNextStep(ctx, store, resolvedIDs[0], autoClaim, actor)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not advance to next step: %v\n", err)
			} else if result != nil {
				if jsonOutput {
					outputJSON(map[string]interface{}{
						"completed": completedIssues,
						"continue":  result,
					})
					return
				}
				PrintContinueResult(result)
			}
		}

		// Handle --claim-next flag
		var claimedNextIssue *types.Issue
		if claimNext && completedCount > 0 && !continueFlag {
			readyIssues, err := store.GetReadyWork(ctx, types.WorkFilter{
				Status:     "open",
				Limit:      1,
				SortPolicy: types.SortPolicy("priority"),
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not get ready issues: %v\n", err)
			} else if len(readyIssues) > 0 {
				nextIssue := readyIssues[0]
				err := store.ClaimIssue(ctx, nextIssue.ID, actor)
				if err == nil {
					claimedNextIssue = nextIssue
					if jsonOutput {
						// JSON handled below
					} else {
						fmt.Printf("%s Auto-claimed next ready issue: %s (P%d)\n", ui.RenderPass("✓"), formatFeedbackID(nextIssue.ID, nextIssue.Title), nextIssue.Priority)
					}
					SetLastTouchedID(nextIssue.ID)
				} else {
					fmt.Fprintf(os.Stderr, "Warning: could not claim next issue %s: %v\n", nextIssue.ID, err)
				}
			} else if !jsonOutput {
				fmt.Printf("\n%s No ready issues available to claim.\n", ui.RenderWarn("✨"))
			}
		}

		if jsonOutput && len(completedIssues) > 0 {
			if claimedNextIssue != nil {
				outputJSON(map[string]interface{}{
					"completed": completedIssues,
					"claimed":   claimedNextIssue,
				})
			} else {
				outputJSON(completedIssues)
			}
		}

		totalAttempted := len(resolvedIDs) + len(routedArgs)
		if totalAttempted > 0 && completedCount == 0 {
			os.Exit(1)
		}
	},
}

func init() {
	completeCmd.Flags().StringP("reason", "r", "", "Reason for completion")
	completeCmd.Flags().String("resolution", "", "Alias for --reason")
	_ = completeCmd.Flags().MarkHidden("resolution")
	completeCmd.Flags().StringP("message", "m", "", "Alias for --reason")
	_ = completeCmd.Flags().MarkHidden("message")
	completeCmd.Flags().String("comment", "", "Alias for --reason")
	_ = completeCmd.Flags().MarkHidden("comment")
	completeCmd.Flags().BoolP("force", "f", false, "Force completion of pinned issues or unsatisfied gates")
	completeCmd.Flags().Bool("continue", false, "Auto-advance to next step in molecule")
	completeCmd.Flags().Bool("no-auto", false, "With --continue, show next step but don't claim it")
	completeCmd.Flags().Bool("suggest-next", false, "Show newly unblocked issues after completing")
	completeCmd.Flags().Bool("claim-next", false, "Automatically claim the next highest priority available issue")
	completeCmd.Flags().String("session", "", "Claude Code session ID (or set CLAUDE_SESSION_ID env var)")
	completeCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(completeCmd)
}

func validateIssueCompletatable(id string, issue *types.Issue, force bool) error {
	if issue == nil {
		return fmt.Errorf("issue %s not found", id)
	}
	if issue.Status == types.StatusClosed {
		return fmt.Errorf("issue %s is already closed", id)
	}
	if issue.Status == types.StatusCompleted {
		return fmt.Errorf("issue %s is already completed", id)
	}
	if issue.Pinned && !force {
		return fmt.Errorf("issue %s is pinned (context marker, not a work item); use --force to complete", id)
	}
	return nil
}
