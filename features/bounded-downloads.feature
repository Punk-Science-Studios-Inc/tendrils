Feature: A download can neither hang a pass nor exhaust memory
  As the owner of devices syncing over an unreliable home network,
  I want a stalled or malformed download abandoned and retried,
  so that one bad transfer cannot stop every other file from syncing.

  # Client:  internal/blob/bounded_test.go
  # Engine:  internal/engine/journal_test.go

  Scenario: The body stops arriving
    Given a download is in progress
    When no bytes arrive for the stall timeout
    Then the transfer is abandoned as transient
    And the next configured server is tried
    And the destination is untouched and the file stays pending

  Scenario: A slow transfer that keeps moving
    When bytes keep arriving, however slowly
    Then the transfer is never cut off

  Scenario Outline: The server sends the wrong amount
    Given the event says the blob is N bytes
    When the server <sends>
    Then the download is refused before anything is written

    Examples:
      | sends                                  |
      | declares a different Content-Length    |
      | sends more than N bytes undeclared     |
      | sends fewer than N bytes undeclared    |
      | closes the connection half way         |
      | sends N bytes that do not hash right   |

  Scenario: A blob of unknown size
    When the size is not known in advance
    Then at most MaxBytes are read
