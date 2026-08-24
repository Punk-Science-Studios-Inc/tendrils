Feature: Knowing this device is behind, and upgrading it safely
  As someone whose devices sync with each other over a shared protocol,
  I want a stale device to tell me it is stale and to upgrade itself on request,
  so that a fleet cannot drift apart in ways no single device can detect.

  Rule: A device says when it is behind, without ever getting in the way

    Scenario: An ordinary command carries the notice
      Given a newer release exists
      When Sam runs any Tendrils command
      Then the command prints its own output first
      And a short notice says which version is available and what to run

    Scenario: The check never delays a command
      Given the check for a newer release has not run today
      When Sam runs a Tendrils command
      Then the command completes without waiting for the network
      And the check runs in the background for the next command to read

    Scenario: Being offline is silent
      Given the device has no network connection
      When Sam runs a Tendrils command
      Then no update error is printed
      And the failed check is retried later on a growing delay

    Scenario: A device on the latest release says nothing
      Given this device runs the newest published release
      When Sam runs a Tendrils command
      Then no notice is printed

    Scenario: The repository has published no releases yet
      Given no release has ever been published
      When Sam asks whether an upgrade is available
      Then Tendrils reports that nothing has been published
      And it does not claim the device is up to date
      And it does not report an error

  Rule: The owner can turn the check off completely

    Scenario: Opting out in the environment
      Given TENDRILS_NO_UPDATE_CHECK is set
      When Sam runs a Tendrils command
      Then no network request is made for an update check
      And no notice is printed

    Scenario: Opting out in the config file
      Given "update_check" is false in the local config
      When Sam runs a Tendrils command
      Then no network request is made for an update check

  Rule: An upgrade installs only what it can verify

    Scenario: The download is verified against the release checksums
      When Sam runs the upgrade command
      Then the release archive is checked against the release's checksums file
      And the new binary is run and asked its version before anything is replaced

    Scenario: A corrupted download is refused
      Given the release archive does not match its published checksum
      When Sam runs the upgrade command
      Then nothing is installed
      And the working binary is left exactly as it was

    Scenario: An archive with no checksum entry is refused
      Given the release checksums do not list the archive for this platform
      When Sam runs the upgrade command
      Then Tendrils refuses to install it
      And it says the download could not be verified

    Scenario: A binary that does not run is refused
      Given the downloaded binary does not report the version it was published as
      When Sam runs the upgrade command
      Then nothing is replaced
      And the working binary is left exactly as it was

    Scenario: An interrupted upgrade leaves a working binary
      Given the replacement fails partway through
      When Sam looks at the installed binary
      Then it is the previous working version

  Rule: The upgrade replaces what is installed, and nothing else

    Scenario: The blob server is upgraded only where it is already installed
      Given blossomd sits beside tendrils on this device
      When Sam runs the upgrade command
      Then both binaries are replaced from the same release
      And the blob server is replaced before the sync client

    Scenario: A device without the blob server does not gain one
      Given only tendrils is installed on this device
      When Sam runs the upgrade command
      Then only tendrils is replaced
      And no blob server is installed

    Scenario: Upgrading while the daemon is running
      Given the daemon is running from the binary being replaced
      When Sam runs the upgrade command
      Then the running daemon is undisturbed
      And Tendrils prints the command to restart it on this platform

    Scenario: The daemon never upgrades itself
      Given the daemon is running and a newer release exists
      Then it reports the newer release in its startup banner and its log
      And it does not download, install, or restart anything

  Rule: A release that changes the wire format is a fleet-wide decision

    Scenario: The notice says a release must be taken together
      Given the newer release declares a different wire format
      When the notice is printed
      Then it says every device must be upgraded together

    Scenario: Crossing the boundary requires an explicit act
      Given the newer release declares a different wire format
      When Sam runs the upgrade command
      Then Tendrils refuses to install it without an explicit confirmation
      And a non-interactive run refuses rather than assuming consent

    Scenario: A release that declares nothing is not treated as a boundary
      Given the newer release declares no wire format
      When Sam runs the upgrade command
      Then no coordinated-upgrade confirmation is required

    Scenario: A boundary marker that cannot be read is treated as a boundary
      Given the newer release publishes an unreadable wire-format marker
      When Sam runs the upgrade command
      Then Tendrils treats it as a coordinated upgrade

  Rule: A specific version can be installed, including an older one

    Scenario: Rolling back to a previous release
      Given a bad release is installed
      When Sam upgrades with an explicit older version
      Then that exact release is installed
      And Tendrils says plainly that this is a downgrade

    Scenario: A version that was never published
      When Sam asks for a version that does not exist
      Then Tendrils reports that no such release exists
      And nothing is installed

  Rule: The check can be asked directly, for scripts and monitoring

    Scenario: A device that is behind reports it in its exit status
      Given a newer release exists
      When Sam runs the check-only form of the upgrade command
      Then it names the available release
      And it exits with a non-zero status

    Scenario: A device that is current exits zero
      Given this device runs the newest published release
      When Sam runs the check-only form of the upgrade command
      Then it reports that the device is current
      And it exits zero
