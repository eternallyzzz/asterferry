# Optional GeoIP routing data

GeoIP is an optional routing input. AsterFerry does not commit a GeoIP binary
to source control or embed one in the container image. The current repository
does not have verified provenance and licensing for its former `cn.mmdb`, so
that file is intentionally absent from the v2.0 release inputs.

Supply a reviewed MaxMind-compatible country database through an external,
versioned resource. Keep the exact resource version and SHA-256 digest in the
deployment record (and, when published by the project, in the release
manifest). Do not download data from the Node process at runtime.

Native Node example:

```powershell
asterferry node run --bootstrap .\state\bootstrap.json --geoip-db .\geoip\cn.mmdb
```

The resolver loads the file lazily, requires a regular file, and rejects data
older than 180 days by default. Missing, disabled, invalid or stale data makes
only country-code rules unavailable; direct, CIDR, domain and `private` rules
continue to work. `geoip_up=0` is reported in Node observed metrics so the
operator can distinguish fallback operation from a loaded database.

For Helm, put the reviewed file in an operator-owned ConfigMap and set
`geoip.enabled=true` and `geoip.existingConfigMap=<name>` on the Node chart.
The ConfigMap is mounted read-only. Treat the database as licensed third-party
data and refresh it as part of the release/deployment process.
