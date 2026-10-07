Feature: An interrupted write or trash is finished, never half-done
  As the owner of a folder synced across devices,
  I want a crash, power cut or full disk in the middle of a pull or a deletion
  to leave my files and the device's record of them consistent,
  so that a restart picks up where it stopped and nothing is lost or misreported.

  # Index and schema:    internal/index/journal_test.go
  # Crash matrix:        internal/engine/crash_test.go (child process, fault per stage)
  # Recovery rules:      internal/engine/journal_test.go
  # Disk full:           internal/engine/mount_linux_test.go (-tags mountintegration)
  # Sharing violations:  internal/engine/locked_windows_test.go
  # Status:              cmd/tendrils/status_test.go

  Rule: The record of a file changes only after the file itself is in place

    Scenario Outline: The daemon dies part-way through a pull
      Given "f.md" is synced and a newer <kind> version is on the relay
      When the daemon is killed after it has <stage>
      And it is killed again at the same point while recovering
      And it is then restarted
      Then "f.md" holds the newer version
      And the device's record of "f.md" names the newer version
      And no staging file is left in the folder

      Examples:
        | kind    | stage                               |
        | small   | staged the download                 |
        | small   | journaled the pull                  |
        | small   | moved the download into place       |
        | chunked | staged the download                 |
        | chunked | journaled the pull                  |
        | chunked | moved the download into place       |

    Scenario Outline: The daemon dies while a losing edit is being preserved
      Given Sam edited "f.md" and a newer remote version wins
      When the daemon is killed after it has <stage>, twice, and restarted
      Then "f.md" holds the remote version
      And exactly one conflict copy holds Sam's edit

      Examples:
        | stage                         |
        | staged the download           |
        | journaled the pull            |
        | made the conflict copy        |
        | moved the download into place |

    Scenario Outline: The daemon dies part-way through applying a deletion
      Given another device deleted "f.md"
      When the daemon is killed after it has <stage>, twice, and restarted
      Then "f.md" is in the trash exactly once
      And the device records "f.md" as deleted

      Examples:
        | stage                 |
        | journaled the trash   |
        | moved it to the trash |

  Rule: Recovery acts only on evidence

    Scenario: The staged download disappeared and the file does not hold it
      Given an interrupted pull whose staging file is gone
      And "a.md" does not hold the downloaded version
      When the daemon restarts
      Then "a.md" is left as it is
      And the pull is dropped and decided again from what is on disk

    Scenario: A record written by a newer build
      Given the journal holds an operation this build does not understand
      When the daemon runs
      Then the operation is kept, its staging file is kept, and its path is left alone

    Scenario: A download cut off before it was journaled
      Given a staging file no journal record owns
      When the daemon runs
      Then the staging file is removed

  Rule: Failures leave the original and keep the work pending

    Scenario: The disk fills while a download is staged
      When staging a pull runs out of space
      Then the original file is untouched
      And nothing is journaled and no staging file remains

    Scenario: The disk fills while the losing version is preserved
      When preserving a conflict copy runs out of space
      Then the original file is untouched and the verified download is kept
      And the pull finishes once there is room

    Scenario: Another program holds the file open on Windows
      Given a program holds "a.md" open without delete sharing
      When a pull replaces it
      Then the replacement is retried a bounded number of times
      And if the lock outlasts that, "a.md" is untouched and the pull stays pending
      And it completes on a later pass once the lock is gone

  Rule: State from older and newer builds

    Scenario: An index from an older build
      When this build opens it
      Then its synced, deleted and retry records are kept and a journal is added

    Scenario: An index from a newer build
      When this build opens it
      Then it refuses rather than guessing

    Scenario: Status while an operation is unfinished
      When Sam runs "tendrils status"
      Then it reports the number of interrupted operations
