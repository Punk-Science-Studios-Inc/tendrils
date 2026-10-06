Feature: A missing or unreadable folder is never mistaken for deleted files
  As the owner of a folder synced across my own devices,
  I want a device that cannot see its folder to stop rather than guess,
  so that an unmounted drive or an unreadable subfolder never deletes my files
  everywhere else.

  # Engine scenarios: internal/engine/rootguard_test.go
  # Real mounts:      internal/engine/mount_linux_test.go (-tags mountintegration)
  # CLI scenarios:    cmd/tendrils/rootguard_test.go
  Background:
    Given Sam's desktop has synced "a.md" and "notes/b.md" from its folder

  Rule: Work pauses unless the folder is the one that was enrolled

    Scenario: The folder is missing
      When Sam's desktop folder is no longer there
      Then the desktop pauses with the reason shown in status
      And no deletion is published

    Scenario: An unmounted drive leaves an empty mountpoint
      When the drive holding Sam's folder is unmounted, leaving an empty directory
      Then the desktop pauses because the folder's marker is missing
      And no deletion is published

    Scenario: A different folder is mounted in its place
      When another enrolled folder is mounted at the same path
      Then the desktop pauses because the marker belongs to a different enrollment

    Scenario: The folder comes back
      Given the desktop paused because its folder was missing
      When the folder returns
      Then syncing resumes
      And nothing is republished and nothing is tombstoned

    Scenario: The drive is pulled during a pass
      Given a pass is publishing two real deletions
      When the folder disappears after the first
      Then the second is not published and the pass pauses

    Scenario: A real deletion still propagates
      When Sam deletes "a.md" on the desktop
      Then a deletion for "a.md" is published

  Rule: Only what was actually observed can be deleted

    Scenario: A subfolder cannot be read
      Given "notes" is unreadable on the desktop
      When the desktop syncs
      Then "notes/b.md" is reported unavailable, not deleted
      And the rest of the folder keeps syncing

    Scenario: A remote change lands in an unreadable subfolder
      Given "notes" is unreadable on the desktop
      When Sam's laptop adds "notes/c.md"
      Then the desktop does not write "notes/c.md" until "notes" can be read

    Scenario: A synced file is replaced by a link
      When "a.md" becomes a symbolic link
      Then it is reported unavailable and no deletion is published

    Scenario: A drive mounted inside the folder
      Given a drive is mounted at "media" inside Sam's folder
      Then nothing under "media" is synced and it is reported unavailable
      And when the drive is unmounted nothing under "media" is deleted

  Rule: Excluded paths are never opened, and unreadable rules stop the pass

    Scenario: An excluded folder Sam cannot read
      Given Sam excludes "private/" on the desktop and cannot read it
      When the desktop syncs
      Then "private/" is not opened and nothing is reported unavailable

    Scenario: A re-included path beneath an ignored folder
      Given the shared rules ignore "music/" and the desktop re-includes "music/notes/"
      Then files in "music/notes/" sync and the rest of "music/" does not

    Scenario: The ignore rules cannot be read
      When ".tendrilsignore" becomes unreadable on the desktop
      Then the desktop pauses rather than syncing with unknown rules

  Rule: Older enrollments are adopted explicitly

    Scenario: The daemon meets an enrollment with no recorded folder identity
      Given Sam's laptop was enrolled by an older build
      When its daemon starts
      Then it refuses and asks Sam to run "tendrils adopt"

    Scenario: Adopting an empty mountpoint is refused
      Given the laptop's folder holds none of the files it last synced
      When Sam runs "tendrils adopt"
      Then adoption is refused and nothing is marked

    Scenario: Adopting the real folder
      Given the laptop's folder holds the files it last synced
      When Sam runs "tendrils adopt"
      Then the folder is marked and the daemon can start
      And no file is removed
