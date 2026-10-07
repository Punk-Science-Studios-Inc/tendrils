Feature: A pull never discards a change it did not plan for
  As the owner of a folder synced across devices,
  I want a download to replace only the file it was planned against,
  and every losing version kept under a name of its own,
  so that no edit is lost to a slow transfer or a repeated conflict.

  # Boundary:         internal/rootfs/conditional_test.go, locked_windows_test.go
  # Engine scenarios: internal/engine/preserve_test.go

  Rule: A destination that changed during a pull is decided again, not replaced

    Scenario: A file is edited while a newer remote version downloads
      Given "c.md" is synced on Sam's desktop
      And a newer "c.md" from his laptop is downloading
      When Sam edits "c.md" on the desktop before the download finishes
      Then his edit is still in "c.md" after the pass
      And the pass is not recorded as a failure
      And the next pass keeps both versions, one as a conflict copy

    Scenario: A file is created where a new remote file is downloading
      Given "n.md" from the laptop is downloading to the desktop
      When Sam creates his own "n.md" on the desktop before it finishes
      Then his "n.md" is not overwritten
      And the next pass keeps both versions

    Scenario: A file is deleted while a newer remote version downloads
      Given "d.md" is synced on Sam's desktop
      And a newer "d.md" is downloading
      When Sam deletes "d.md" on the desktop before it finishes
      Then "d.md" is not recreated by that pass

    Scenario: A file is edited after the scan that chose to trash it
      Given a remote deletion of "t.md" is about to be applied
      When Sam edits "t.md" first
      Then "t.md" is left in place

    Scenario: Another process holds the destination open on Windows
      Given a program holds "locked.md" open without delete sharing
      When a pull tries to replace it
      Then the original is untouched and the work stays pending

  Rule: Every losing version gets a name of its own

    Scenario: The same path loses three conflicts in one second
      Given two machines share one owner key
      When "r.md" loses to a newer remote version three times
      Then three conflict copies exist, each holding one losing version

    Scenario: A preservation name is already taken
      Given a file already exists at the chosen conflict-copy name
      When a losing version is preserved
      Then the existing file is not overwritten
      And another name is chosen

    Scenario: A large losing file
      Given a 64 MiB file loses a conflict
      Then it is copied without loading it into memory
