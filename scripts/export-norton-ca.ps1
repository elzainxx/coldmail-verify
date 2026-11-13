# Export the Norton SSL/TLS-scanning root CA to a PEM in the verifier-node build context,
# so the container trusts the same interception proxy the host does (security-preserving:
# we ADD trust for the proxy, we do NOT disable TLS verification).
#
# NOT a one-off: run this before EVERY verifier-node rebuild on a TLS-interception host. The proxy
# rotates its CA key while keeping an identical subject/issuer name, so a stale PEM looks correct and
# fails anyway — in-container `bundle install` dies on CertificateFailureError and the RSpec gate
# exits 17 with NO output at all. Documented, with the SKI/AKI confirmation and why RSpec is the only
# gate affected, in Docs/install-and-deploy.md §2 "Prerequisite on a TLS-interception host". This header used
# to say "one-off build helper", which is how the same diagnosis got paid for twice.
#
# Writes LF-only, 64-col-wrapped PEM (update-ca-certificates / OpenSSL c_rehash reject
# CRLF and malformed wrapping — that silently registers the file but emits empty content).
$ErrorActionPreference = 'Stop'
$cert = Get-ChildItem -Path Cert:\LocalMachine\Root, Cert:\CurrentUser\Root |
  Where-Object { $_.Subject -like '*Norton Web/Mail Shield Root*' } |
  Select-Object -First 1
if (-not $cert) { Write-Error 'Norton root CA not found'; exit 1 }

$b64 = [System.Convert]::ToBase64String($cert.RawData)   # one unbroken base64 string
# Wrap at 64 chars, join with LF only.
$lines = @()
for ($i = 0; $i -lt $b64.Length; $i += 64) {
  $len = [Math]::Min(64, $b64.Length - $i)
  $lines += $b64.Substring($i, $len)
}
$body = ($lines -join "`n")
$pem = "-----BEGIN CERTIFICATE-----`n$body`n-----END CERTIFICATE-----`n"

$out = Join-Path $PSScriptRoot '..\verifier-node\truemail-rack\certs\norton-ssl-proxy-ca.crt'
# Write raw bytes with LF only (Set-Content would inject CRLF on Windows).
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
[System.IO.File]::WriteAllText($out, $pem, $utf8NoBom)
Write-Output "exported LF-only PEM to $out"
