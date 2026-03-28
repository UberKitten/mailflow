<#
.SYNOPSIS
Lookup an envelope recipient (SMTP RCPT TO) via Exchange Online message trace.

.DESCRIPTION
This script is intended for mailflow's optional envelope lookup feature.
It accepts the Internet Message-ID, sender address, and received timestamp,
then performs a message trace query and prints the recipient address on stdout.

AUTH REQUIREMENTS
- ExchangeOnlineManagement module installed
- Azure AD app with Exchange.ManageAsApp (application) permission
- App granted Message Trace / Mail Recipients roles
- Certificate-based auth (thumbprint or PFX path)

ENV VARS (optional)
- EXO_APP_ID
- EXO_ORGANIZATION (tenant domain or tenant ID)
- EXO_CERT_THUMBPRINT
- EXO_CERT_PATH
- EXO_CERT_PASSWORD
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0, Mandatory = $true)]
    [string]$MessageId,

    [Parameter(Position = 1, Mandatory = $true)]
    [string]$SenderAddress,

    [Parameter(Position = 2, Mandatory = $true)]
    [string]$ReceivedDate,

    [string]$AppId = $env:EXO_APP_ID,
    [string]$Organization = $env:EXO_ORGANIZATION,
    [string]$CertificateThumbprint = $env:EXO_CERT_THUMBPRINT,
    [string]$CertificatePath = $env:EXO_CERT_PATH,
    [string]$CertificatePassword = $env:EXO_CERT_PASSWORD
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $AppId) {
    throw 'Missing AppId. Provide -AppId or set EXO_APP_ID.'
}
if (-not $Organization) {
    throw 'Missing Organization. Provide -Organization or set EXO_ORGANIZATION.'
}
if (-not $CertificateThumbprint -and -not $CertificatePath) {
    throw 'Missing certificate. Provide -CertificateThumbprint or -CertificatePath.'
}

if (-not (Get-Module -ListAvailable -Name ExchangeOnlineManagement)) {
    throw 'ExchangeOnlineManagement module not found. Install-Module ExchangeOnlineManagement.'
}

$received = try {
    [DateTime]::Parse($ReceivedDate).ToUniversalTime()
} catch {
    throw "Invalid received date: $ReceivedDate"
}

# Search window around received time (adjust if needed)
$windowHours = 12
$startDate = $received.AddHours(-$windowHours)
$endDate = $received.AddHours($windowHours)

$connectParams = @{
    AppId       = $AppId
    Organization = $Organization
    ShowBanner  = $false
}

if ($CertificateThumbprint) {
    $connectParams.CertificateThumbprint = $CertificateThumbprint
} else {
    $connectParams.CertificateFilePath = $CertificatePath
    if ($CertificatePassword) {
        $connectParams.CertificatePassword = ConvertTo-SecureString -String $CertificatePassword -AsPlainText -Force
    }
}

try {
    Connect-ExchangeOnline @connectParams | Out-Null

    $traceCmd = Get-Command Get-MessageTraceV2 -ErrorAction SilentlyContinue
    if ($traceCmd) {
        $results = Get-MessageTraceV2 -MessageId $MessageId -SenderAddress $SenderAddress -StartDate $startDate -EndDate $endDate -PageSize 1000
    } else {
        $results = Get-MessageTrace -MessageId $MessageId -SenderAddress $SenderAddress -StartDate $startDate -EndDate $endDate -PageSize 5000
    }

    if (-not $results) {
        throw 'No message trace results found.'
    }

    $recipient = $results | Select-Object -ExpandProperty RecipientAddress -First 1
    if (-not $recipient) {
        throw 'RecipientAddress not found in trace results.'
    }

    Write-Output $recipient
} catch {
    Write-Error $_
    exit 1
} finally {
    Disconnect-ExchangeOnline -Confirm:$false -ErrorAction SilentlyContinue | Out-Null
}
