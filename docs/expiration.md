# LWE expiration

Set `spec.duration` to a Go `time.Duration` string such as `5h30m`. If omitted,
the operator uses its configured minimum duration. The default accepted range is
`1h` through `720h`; operator administrators can change the bounds with
`LWE_MIN_DURATION` and `LWE_MAX_DURATION` (or the Helm values
`expiration.minDuration` and `expiration.maxDuration`). Both bounds must be
positive Go duration strings, and the minimum cannot exceed the maximum.

The operator records the absolute deadline in
`status.expirationTimestamp`. Ordinary reconciliation does not move it. When
`spec.duration` changes, a valid value schedules a new deadline from the time of
the update. An invalid initial duration prevents namespace creation. An invalid
duration update leaves the existing deadline unchanged and sets
`ExpirationReady=False`; correcting the duration schedules a new deadline.

An administrator can exempt an LWE by setting this annotation on the LWE:

```yaml
metadata:
  annotations:
    llm.redhat.com/expiration-exempt: "true"
```

The expiration timestamp remains visible while exempt. Removing the annotation
after that timestamp has passed makes the LWE immediately eligible for
expiration. An LWE already marked expired is terminal; changing its duration or
adding the exemption does not recreate its namespace.

The operator reviews expiration timestamps every minute. At expiration it sets
the `Expired=True` condition, marks `Ready=False`, and deletes the owned backing
namespace.
