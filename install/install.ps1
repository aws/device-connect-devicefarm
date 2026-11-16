# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Installs Device Connect for AWS Device Farm (devicefarm-connect) on Windows, from PowerShell:
#
#   irm https://github.com/aws/device-connect-devicefarm/releases/latest/download/install.ps1 | iex
#
# Installs only the release that this file is part of.
#
# Environment:
#   DEVICEFARM_CONNECT_INSTALL_DIR       Install folder. Default: %USERPROFILE%\.local\bin
#   DEVICEFARM_CONNECT_INSTALL_BASE_URL  Download source, for a mirror. Default: the GitHub releases
#
# Checks, in this order. A failed check stops the install:
#   1. OpenPGP signature of SHA256SUMS. Skipped with a notice when gpg.exe or gpgv.exe
#      is missing, or when this file holds no release key.
#   2. SHA-256 checksum of the program against SHA256SUMS.
#   3. Authenticode signature of the program, and its signer.
#
# Needs no administrator rights. Changes no registry value and no saved PATH. Never runs the program.

# The script block keeps variables and settings out of the caller's session.
& {
    # Parameter defaults of the caller, such as '*:ErrorAction', must not hide a failure.
    $PSDefaultParameterValues = @{}
    Set-StrictMode -Version 3.0
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'

    $version = 'v1.0.0'
    # The armored release public key goes between the next two lines. No key: no signature check.
    $releaseKey = @'
-----BEGIN PGP PUBLIC KEY BLOCK-----
Comment: 492C 96C0 8389 3C96 0BA4  86DE 236A E7FF FA0D 086E
Comment: Device Connect for AWS Device Farm <aws-devicefarm-supp

xsFNBGr8hMwBEACQTMhDZ0tuEkysoiZZXcCquLJKlbuVP4rUFKSi4qofc3TlMk2g
Q1DjMsSknZhBg+oYFxXWvhbr43WK8TL7sv9LlEAUR4jZzKQqpmCLqMc6uxEX6tuX
tuob3N6iEe6ODo1WG5XTvJCuHF6w+2al8pVVpJ+rkPHchTTqr2DM3z4SBBTPzPNG
5YLYtOx4+Ezah73Rk8eDF19WZYa0V6ROlPMkMW1As6mSlXJZ0zdueiICwBee7k1Q
wzdFfbGMDws25sMAuXEJDDBKmAsjCUK8l9okBkZryCuJ5y8bApEwWHNNtqCHIZr2
nfLCqu8nvTPpQRa8ls9QF0SULHqeDYuQ3SXZVRnwp0q2LvneV0yz4llEQhhoIMc4
iACJl8lbYzQT5TECbebtV3BaxxPmtQPxbr7pYIwqJJbsuAVuDr6OiggUXI2pNykf
RY5Px+dKdaxlZUeZhR7Ws5FZF52McMLwZQbqcOaGb1VyQpT7VgnSrvUUVxRZowOj
VKgGJbir4eOtdil0azRtvA15gO5jkFRy0Yx9WpD1h9pwnUxfUR3ywpnCroeiWZ8o
8fHozYyHm/4t8pcwH6Tzuhc9ggBdzdGVyn1Jp4vnMWPfspMJqTR6COOyeosCU+8d
vpxc8xtNdbZswirqfkz+pq/h3EqL/c72N8oKRsPJkXjOJi4dkfWbOVgQ8QARAQAB
zUZEZXZpY2UgQ29ubmVjdCBmb3IgQVdTIERldmljZSBGYXJtIDxhd3MtZGV2aWNl
ZmFybS1zdXBwb3J0QGFtYXpvbi5jb20+wsG+BBMBCgByBYJq/lh5CRAjauf/+g0I
bkcUAAAAAAAeACBzYWx0QG5vdGF0aW9ucy5zZXF1b2lhLXBncC5vcmelZsyeD18R
C6+TbEYMAOqoWPoQosEmqdc8WWm3sTPW4QKbAxYhBEkslsCDiTyWC6SG3iNq5//6
DQhuAAD/cw/+MTRDSXrUfRl7k2LOcHuezQ8wOLg96+Yr2y/uTlELpsvrRAxmT/Il
Y8ma6h9mgy5Xk+VlbrOY9FPD5HN4a3j0IHrCJFX0ruUyUgus6lf+GejNwkXh2/Z/
qwDJDPcOAHLm6f/jvcFxK8DnTEG4ZfkWgXH7Epqok4nAzmdrvDHTnCLDsQj/4gi7
RjSQsD0OdGMr3X9kaGLEvNibwpUUvXcQd2WCerW3OOIWbs+ruW8SyHzYQcWsat6N
mAmhVqp0q0HOVDJ0Tr4vXRISXbZ4KpDXbOaIAeHzTauR+RRJehz7ZoWQA+QcT062
r+goT7WI691VCYGlo3UdC8Hkroo4490Hp2RE/pMRbIv7v71lKd60/pQ9IhB1vuI5
16SNtFsFosx1pg88AGW/f6HtAwI2Iowrl2EQqX9Wa5+7R2uArIdT35P0jXFY3mAt
p1RStDtSbiKdyOQVwZZ710tEh6gp0C+psSpVtuoTHcnFJXjRDujMZAVxWddCrW8Y
J37f5H0FQrbQGJLJxptkJgrggVWVWUbe7DBRqqZLL2AD3q1OTyVD9eby2m0mOAu5
/HVcBBtT85iuSF1Ie+FVcimK4WWODnKuA/yTRsEnw7sjNx7U48EEEfunsmEtncxB
Rrqtp8537D/2lxbsri9Zb1p4kHHcQJ6Gs/f3v2sJ2lqdRUpW+39r4L0=
=w0bv
-----END PGP PUBLIC KEY BLOCK-----
'@
    $programFile = 'devicefarm-connect_windows_amd64.exe'
    $releasesUrl = 'https://github.com/aws/device-connect-devicefarm/releases'
    $signerOrganization = 'Amazon Web Services, Inc.'
    $signerUnit = 'AWS Dev Tools'

    # No call operator: Windows PowerShell 5.1 can turn stderr text into an error, and $LASTEXITCODE
    # would change for the caller.
    function Invoke-Program {
        param([string]$FilePath, [string]$Arguments, [string]$WorkingDirectory)
        $startInfo = New-Object System.Diagnostics.ProcessStartInfo
        $startInfo.FileName = $FilePath
        $startInfo.Arguments = $Arguments
        $startInfo.WorkingDirectory = $WorkingDirectory
        $startInfo.UseShellExecute = $false
        $startInfo.CreateNoWindow = $true
        $startInfo.RedirectStandardInput = $true
        $startInfo.RedirectStandardOutput = $true
        $startInfo.RedirectStandardError = $true
        $process = [System.Diagnostics.Process]::Start($startInfo)
        try {
            $process.StandardInput.Close()
            # Read both streams at the same time, or a full pipe can block the program.
            $standardOutput = $process.StandardOutput.ReadToEndAsync()
            $standardError = $process.StandardError.ReadToEndAsync()
            $process.WaitForExit()
            return [pscustomobject]@{
                ExitCode = $process.ExitCode
                Lines    = @(($standardOutput.Result + "`n" + $standardError.Result) -split '\r?\n' | Where-Object { $_ })
            }
        } finally {
            $process.Dispose()
        }
    }

    # One retry: a request can fail one time for a network reason.
    function Save-Download {
        param([string]$Url, [string]$FilePath)
        foreach ($attempt in 1, 2) {
            try {
                Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $FilePath -TimeoutSec 60
                return
            } catch {
                if ($attempt -eq 2) {
                    throw "The download of $Url failed: $($_.Exception.Message.Trim())"
                }
            }
        }
    }

    # Parses the subject: a text search for 'O=...' is not safe, because a CN can hold that text.
    # Windows quotes a value that holds a comma, a line break or a quotation mark, and doubles a
    # quotation mark inside. A subject that does not parse gives no values.
    function Get-SubjectValues {
        param([string]$Subject, [string]$Attribute)
        $attributePattern = [regex]'\G(?<name>[A-Za-z0-9.]+)=(?:"(?<quoted>(?:[^"]|"")*)"|(?<plain>[^",+]*?))(?:, | \+ |\z)'
        $values = @()
        $position = 0
        while ($position -lt $Subject.Length) {
            $attributeMatch = $attributePattern.Match($Subject, $position)
            if (-not $attributeMatch.Success) {
                return @()
            }
            if ([string]::Equals($attributeMatch.Groups['name'].Value, $Attribute, [StringComparison]::Ordinal)) {
                if ($attributeMatch.Groups['quoted'].Success) {
                    $values += $attributeMatch.Groups['quoted'].Value.Replace('""', '"')
                } else {
                    $values += $attributeMatch.Groups['plain'].Value
                }
            }
            $position = $attributeMatch.Index + $attributeMatch.Length
        }
        return $values
    }

    $temporaryFolder = $null
    try {
        if ($ExecutionContext.SessionState.LanguageMode -ne 'FullLanguage') {
            throw "The installer needs the FullLanguage mode of PowerShell. See $releasesUrl for the release files."
        }
        if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
            throw 'This installer is for Windows only. On macOS or Linux, use install.sh.'
        }
        # A 32-bit PowerShell on 64-bit Windows has the true architecture in PROCESSOR_ARCHITEW6432.
        $architecture = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
        if ($architecture -ne 'AMD64') {
            throw "No release file exists for Windows on $architecture. See $releasesUrl for the release files."
        }
        $build = [Environment]::OSVersion.Version.Build
        if ($build -lt 17063) {
            throw "This program needs Windows 10 build 17063 or later. This computer has build $build."
        }
        # PowerShell 7 cannot load this module when its execution policy is Restricted or AllSigned.
        try {
            Import-Module Microsoft.PowerShell.Security
        } catch {
            throw 'This PowerShell cannot load the module Microsoft.PowerShell.Security. Run the installer in Windows PowerShell (powershell.exe).'
        }

        $installFolder = $env:DEVICEFARM_CONNECT_INSTALL_DIR
        if (-not $installFolder) {
            if (-not $env:USERPROFILE) {
                throw 'USERPROFILE is not set. Set DEVICEFARM_CONNECT_INSTALL_DIR to the install folder.'
            }
            $installFolder = Join-Path $env:USERPROFILE '.local\bin'
        }
        $installFolder = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($installFolder)
        $destination = Join-Path $installFolder 'devicefarm-connect.exe'
        if (Test-Path -LiteralPath $destination -PathType Container) {
            throw "$destination is a folder. Remove it, then run the installer again."
        }

        $baseUrl = $env:DEVICEFARM_CONNECT_INSTALL_BASE_URL
        if (-not $baseUrl) {
            $baseUrl = "$releasesUrl/download"
        }
        $releaseUrl = "$($baseUrl.TrimEnd('/'))/$version"

        $temporaryFolder = Join-Path ([IO.Path]::GetTempPath()) "devicefarm-connect-install-$([Guid]::NewGuid().ToString('N'))"
        $null = New-Item -ItemType Directory -Path $temporaryFolder

        Write-Host "Downloading devicefarm-connect $version ($programFile)."
        $checksumList = Join-Path $temporaryFolder 'SHA256SUMS'
        Save-Download "$releaseUrl/SHA256SUMS" $checksumList

        $gpg = Get-Command gpg.exe -CommandType Application -ErrorAction Ignore | Select-Object -First 1
        $gpgv = Get-Command gpgv.exe -CommandType Application -ErrorAction Ignore | Select-Object -First 1
        if (-not $releaseKey.Trim()) {
            Write-Host 'The signature check of SHA256SUMS was skipped. This installer has no release key.'
        } elseif (-not $gpg) {
            Write-Host 'The signature check of SHA256SUMS was skipped. gpg was not found.'
        } elseif (-not $gpgv) {
            Write-Host 'The signature check of SHA256SUMS was skipped. gpgv was not found.'
        } else {
            Save-Download "$releaseUrl/SHA256SUMS.sig" (Join-Path $temporaryFolder 'SHA256SUMS.sig')
            # Own GnuPG home: gpg stays away from the keys and settings of the user.
            # Relative names, run from the temporary folder: a gpgv from Cygwin or MSYS accepts no
            # keyring path that holds a colon.
            $null = New-Item -ItemType Directory -Path (Join-Path $temporaryFolder 'gnupg')
            Set-Content -LiteralPath (Join-Path $temporaryFolder 'release-key.asc') -Value $releaseKey -Encoding ASCII
            $dearmor = Invoke-Program $gpg.Path '--homedir gnupg --batch --no-tty --no-options --yes --output release-key.gpg --dearmor release-key.asc' $temporaryFolder
            if ($dearmor.ExitCode -ne 0) {
                $dearmor.Lines | ForEach-Object { Write-Host $_ }
                throw 'The release key in this installer is not valid.'
            }
            $verification = Invoke-Program $gpgv.Path '--homedir gnupg --keyring ./release-key.gpg SHA256SUMS.sig SHA256SUMS' $temporaryFolder
            if ($verification.ExitCode -ne 0) {
                $verification.Lines | ForEach-Object { Write-Host $_ }
                throw 'The signature of SHA256SUMS is not good.'
            }
            Write-Host 'The signature of SHA256SUMS is good.'
        }

        $program = Join-Path $temporaryFolder $programFile
        Save-Download "$releaseUrl/$programFile" $program

        $checksumPattern = '^[0-9a-f]{64}  ' + [regex]::Escape($programFile) + '$'
        $checksumLines = @(Get-Content -LiteralPath $checksumList | Where-Object { $_ -cmatch $checksumPattern })
        if ($checksumLines.Count -eq 0) {
            throw "SHA256SUMS has no line for $programFile."
        }
        if ($checksumLines.Count -gt 1) {
            throw "SHA256SUMS has more than one line for $programFile."
        }
        $checksum = (Get-FileHash -LiteralPath $program -Algorithm SHA256).Hash.ToLowerInvariant()
        if (-not [string]::Equals($checksum, $checksumLines[0].Substring(0, 64), [StringComparison]::Ordinal)) {
            throw "The SHA-256 checksum of $programFile is not the checksum in SHA256SUMS."
        }
        Write-Host "The SHA-256 checksum of $programFile is correct."

        $signature = Get-AuthenticodeSignature -LiteralPath $program
        if ($signature.Status -ne 'Valid') {
            throw "The Authenticode signature of $programFile is not valid. Status: $($signature.Status)."
        }
        $signer = $signature.SignerCertificate.SubjectName.Name
        # One O only: a subject with a second organization is not ours. The compares are ordinal,
        # because the -ceq operator ignores some characters, such as a soft hyphen.
        $organizations = @(Get-SubjectValues $signer 'O')
        $units = @(Get-SubjectValues $signer 'OU' | Where-Object { [string]::Equals($_, $signerUnit, [StringComparison]::Ordinal) })
        if (($organizations.Count -ne 1) -or ($units.Count -eq 0) -or
            -not [string]::Equals($organizations[0], $signerOrganization, [StringComparison]::Ordinal)) {
            throw "The signer of $programFile must have O '$signerOrganization' and OU '$signerUnit'. Signer: $signer"
        }
        Write-Host "The Authenticode signature of $programFile is valid. Signer: $signer"

        $null = [IO.Directory]::CreateDirectory($installFolder)
        # Own names: an installer that runs at the same time must not use these files.
        $runId = [Guid]::NewGuid().ToString('N')
        $newFile = "$destination.$runId.new"
        $oldFile = "$destination.$runId.old"
        $replacesInstall = Test-Path -LiteralPath $destination
        $movedInstall = $false
        try {
            # A copy takes the permissions of the install folder. A file moved from the temporary
            # folder keeps the permissions of that folder.
            [IO.File]::Copy($program, $newFile, $true)
            # Windows can rename the file of a running program, but cannot remove it. So the
            # installed file becomes a .old file, and stays while a bridge runs from it.
            if ($replacesInstall) {
                [IO.File]::Move($destination, $oldFile)
                $movedInstall = $true
            }
            [IO.File]::Move($newFile, $destination)
        } catch {
            $failure = $_.Exception.GetBaseException().Message.Trim()
            Remove-Item -LiteralPath $newFile -Force -ErrorAction Ignore
            # Another installer can put its file there first. Then there is nothing to restore.
            if ($movedInstall -and -not (Test-Path -LiteralPath $destination)) {
                try { [IO.File]::Move($oldFile, $destination) } catch { }
            }
            throw "The installer cannot write ${destination}: $failure"
        }
        # Removes the .old files of this install and of earlier ones. A removal fails while a bridge
        # runs from the file, or when another installer removes the file first.
        Get-ChildItem -LiteralPath $installFolder -Filter 'devicefarm-connect.exe.*.old' -File -ErrorAction Ignore |
            ForEach-Object { try { Remove-Item -LiteralPath $_.FullName -Force -ErrorAction Ignore } catch { } }

        Write-Host "Installed devicefarm-connect $version at $destination."
        if ($replacesInstall) {
            Write-Host 'A bridge that runs now keeps the earlier version until you run: devicefarm-connect bridge stop'
        }
        $found = Get-Command devicefarm-connect.exe -CommandType Application -ErrorAction Ignore | Select-Object -First 1
        if (-not $found) {
            # Not safe in the printed commands: a single quotation mark ends the quoted path (PowerShell
            # reads U+2018 to U+201B as one), and a semicolon divides PATH entries.
            if ($installFolder -match "[;'\u2018-\u201B]") {
                Write-Host "$installFolder is not on your PATH. Add it to your PATH, or use the full path of the program."
            } else {
                # Command 1: new terminals. The user PATH becomes REG_SZ, with its variables expanded.
                # Command 2: this terminal. The folder goes last, after the machine PATH, as in a new terminal.
                Write-Host "$installFolder is not on your PATH. To add it, run these commands in PowerShell:"
                Write-Host "  [Environment]::SetEnvironmentVariable('Path', '$installFolder;' + [Environment]::GetEnvironmentVariable('Path', 'User'), 'User')"
                Write-Host "  `$env:Path += ';$installFolder'"
            }
        } elseif (-not [string]::Equals([IO.Path]::GetFullPath($found.Path), [IO.Path]::GetFullPath($destination), [StringComparison]::OrdinalIgnoreCase)) {
            Write-Host "Note: your shell runs $($found.Path), not the new file. Remove that file, or put $installFolder before its folder on your PATH."
        }
    } catch {
        throw "install: error: $($_.Exception.Message.Trim())"
    } finally {
        if ($temporaryFolder) {
            Remove-Item -LiteralPath $temporaryFolder -Recurse -Force -ErrorAction Ignore
        }
    }
}
