Feature: A synced name can never reach outside the folder or overwrite another file
  As the owner of a folder synced across Linux and Windows devices,
  I want every name another device publishes to be checked before anything is
  written, so that a crafted or unrepresentable name cannot touch files outside
  my folder, overwrite a different file, or be deleted for being unwritable.

  # Validator:        internal/syncpath/syncpath_test.go
  # Boundary:         internal/rootfs/rootfs_test.go, junction_windows_test.go
  # Engine scenarios: internal/engine/pathsafety_test.go
  # CLI scenarios:    cmd/tendrils/status_test.go

  Rule: Names that are not canonical relative paths are rejected before mutation

    Scenario Outline: A signed event names a path outside the folder
      Given a signed event for "<path>" is on the relay
      When Sam's desktop syncs
      Then nothing outside the folder changes
      And the root marker and trash are untouched
      And the path is reported as blocked
      And nothing is published in response

      Examples:
        | path                       |
        | ../outside/victim.txt      |
        | a/../../outside/victim.txt |
        | /abs.txt                   |
        | C:/Windows/evil.txt        |
        | .tendrils-root             |
        | .tendrils-trash/a.md       |

  Rule: Operations are confined to the folder at the moment they run

    Scenario: A folder is replaced by a link to somewhere else
      Given "notes" in Sam's folder is a symlink or junction to a directory outside it
      When an event for "notes/new.txt" arrives
      Then nothing is written outside the folder
      And nothing is tombstoned

    Scenario: A folder is swapped for a link between staging and commit
      Given a pull has staged "notes/new.txt" beside its destination
      When "notes" is replaced by a link to outside the folder
      Then the commit is refused

    Scenario: The folder itself is replaced during a pass
      Given a copy of the folder, marker included, is put in its place
      When the next write would commit
      Then the pass pauses and nothing is written to either folder

  Rule: Names this platform cannot represent are blocked, not lost

    Scenario: A Linux device syncs names Windows reserves
      Given Sam's laptop publishes "CON.txt", "report.txt:stream" and "trailing."
      When Sam's Windows desktop syncs
      Then none of them is written on the desktop
      And status reports 3 blocked paths
      And no tombstone is published, so the laptop keeps them

    Scenario: Two names differ only in case
      Given the relay holds "README.md" and "Readme.md"
      When a device with a case-insensitive folder syncs
      Then neither is written there
      And both are reported as blocked

    Scenario: Two folders differ only in case
      Given the relay holds "Docs/a.md" and "docs/b.md"
      When a device with a case-insensitive folder syncs
      Then neither is written there

    Scenario: A rename only changes case
      Given a case-insensitive device has synced "a.txt"
      When another device renames it to "A.txt"
      Then "a.txt" moves to the trash and "A.txt" is written
      And nothing is reported as blocked

  Rule: Legitimate names are not caught by the checks

    Scenario: Non-ASCII and long names
      Given Sam's laptop has "música/日本語/ファイル-<200 x>.flac"
      When the desktop syncs
      Then the desktop holds the same file under the same name
