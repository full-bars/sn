# Deterministic causal controls

1. `causal-restore-current-assumptions.patch` restores the old historical classifier/current-native requirement/phase assumption. Its five-root normal body used private causal binary SHA-256 `2e1e73dcf06eedd43ecbc8418b71212b9617a0facaa0f278bd6c2c355e963a29` and produced three expected semantic failures plus two passes.
   - exit 1; expected failures 3; expected passes 2; actual outcomes and semantic literals matched; converter stderr empty; binary identity unchanged.
   - The expected failures establish the original-native-write, completed-renewal, and later-phase cases. The ordinary-current and matching-cumulative controls pass.
2. `causal-omit-restore-dispatch.patch` removes only historical restore dispatch. Its three-root normal body used private causal binary SHA-256 `2b80c08bf673d08b2a79e0c350c75badefd446e6f72466a0c6fe5e0e6d6cf3f5` and produced one expected constructor failure plus two passes.
   - exit 1; expected failure 1 containing `restore dispatch bypassed historical scope admission`; expected passes 2; actual outcomes and literal matched; converter stderr empty; binary identity unchanged.
   - The original-native-write and ordinary-current controls pass.

Both variants used separate disposable worktrees, exact compiled lists, package CWD, offline frozen dependency projections, and isolated normal binaries. Their expected failures are causal controls, never candidate failures.
