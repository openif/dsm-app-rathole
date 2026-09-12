# build_spk.ps1 - Build Synology DSM SPK Package for Rathole with WebUI
# Cross-platform compatible: runs on Windows PowerShell and Linux pwsh
param (
    [string]$Arch = "rtd1296",
    [string]$Version = "1.0.0-0001",
    [string]$RatholeVersion = "v0.5.0"
)

$ErrorActionPreference = "Stop"
$ROOT_DIR = $PSScriptRoot

if (Test-Path "$ROOT_DIR/spk/INFO") {
    # Called directly inside dsm-app-rathole repo
    $SRC_DIR = $ROOT_DIR
} elseif (Test-Path "$ROOT_DIR/dsm-app-rathole/spk/INFO") {
    # Called from parent workspace
    $SRC_DIR = "$ROOT_DIR/dsm-app-rathole"
} else {
    throw "Cannot locate dsm-app-rathole files!"
}

$BUILD_DIR = "$ROOT_DIR/_build_spk"
$CACHE_DIR = "$ROOT_DIR/_cache"
$OUTPUT_SPK = "$ROOT_DIR/rathole_${Arch}_${Version}.spk"

Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host " Building Synology SPK for Rathole WebUI ($Arch / $Version) " -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan

# Clean build dir
if (Test-Path $BUILD_DIR) { Remove-Item -Recurse -Force $BUILD_DIR }
New-Item -ItemType Directory -Force $BUILD_DIR | Out-Null
New-Item -ItemType Directory -Force $CACHE_DIR | Out-Null

# ---------------------------------------------------------
# Helper: Robust Tar permission fixer for POSIX 0755
# Correctly parses 512-byte headers and skips payload data blocks
# ---------------------------------------------------------
$tarHelperCode = @'
using System;
using System.IO;
using System.Text;

public class SpkTarFixer {
    public static void MakeExecutable(string tarPath) {
        byte[] bytes = File.ReadAllBytes(tarPath);
        int offset = 0;
        while (offset + 512 <= bytes.Length) {
            // End of archive check
            bool isZero = true;
            for (int j = 0; j < 512; j++) {
                if (bytes[offset + j] != 0) { isZero = false; break; }
            }
            if (isZero) break;

            string name = Encoding.ASCII.GetString(bytes, offset, 100).Trim('\0', ' ');
            if (string.IsNullOrEmpty(name)) break;

            // Read file size
            string sizeStr = Encoding.ASCII.GetString(bytes, offset + 124, 12).Trim('\0', ' ');
            long size = 0;
            if (!string.IsNullOrEmpty(sizeStr)) {
                try {
                    size = Convert.ToInt64(sizeStr, 8);
                } catch {
                    size = 0;
                }
            }

            char typeFlag = (char)bytes[offset + 156];
            // Only set executable flag for regular files ('0' or '\0')
            if (typeFlag == '0' || typeFlag == '\0') {
                bool shouldBeExec = name.Contains("start-stop-status") || 
                                    name.Contains("postinst") || 
                                    name.Contains("preinst") || 
                                    name.Contains("preuninst") || 
                                    name.Contains("postuninst") || 
                                    name.Contains("postupgrade") || 
                                    name.EndsWith(".cgi") ||
                                    name.EndsWith("/rathole") || 
                                    name.EndsWith("/rathole-ui");

                if (shouldBeExec) {
                    byte[] mode = Encoding.ASCII.GetBytes("0000755\0");
                    Array.Copy(mode, 0, bytes, offset + 100, 8);

                    // Recalculate checksum (bytes 148-155 treated as spaces 32)
                    for (int c = 0; c < 8; c++) bytes[offset + 148 + c] = 32;
                    int sum = 0;
                    for (int c = 0; c < 512; c++) sum += (int)bytes[offset + c];
                    
                    string chkStr = Convert.ToString(sum, 8).PadLeft(6, '0') + "\0 ";
                    byte[] chkBytes = Encoding.ASCII.GetBytes(chkStr);
                    Array.Copy(chkBytes, 0, bytes, offset + 148, 8);
                }
            }

            // Move offset to next header: header block + data payload blocks
            long contentBlocks = (size + 511) / 512;
            offset += 512 + (int)(contentBlocks * 512);
        }
        File.WriteAllBytes(tarPath, bytes);
    }
}
'@
Add-Type -TypeDefinition $tarHelperCode -ErrorAction SilentlyContinue

function Compress-GzipFile($sourcePath, $destPath) {
    $src = [System.IO.File]::OpenRead($sourcePath)
    $dest = [System.IO.File]::Create($destPath)
    $gz = New-Object System.IO.Compression.GZipStream($dest, [System.IO.Compression.CompressionMode]::Compress)
    $src.CopyTo($gz)
    $gz.Close(); $dest.Close(); $src.Close()
}

$utf8NoBom = New-Object System.Text.UTF8Encoding($false)

# ---------------------------------------------------------
# 1. Compile rathole-ui (Go single binary)
# ---------------------------------------------------------
Write-Host "`n[1/5] Compiling rathole-ui Go Web backend for Linux ($Arch)..." -ForegroundColor Yellow
$goArch = if ($Arch -eq "x86_64") { "amd64" } elseif ($Arch -in @("aarch64", "arm64", "rtd1296")) { "arm64" } else { $Arch }

$env:GOOS = "linux"
$env:GOARCH = $goArch
$env:CGO_ENABLED = "0"

$webUiBin = "$BUILD_DIR/rathole-ui"
Push-Location $SRC_DIR
try {
    go build -ldflags="-s -w" -o $webUiBin ./cmd/rathole-ui
    if ($LASTEXITCODE -ne 0) {
        throw "go build exited with code $LASTEXITCODE"
    }
} finally {
    Pop-Location
}

if (!(Test-Path $webUiBin)) {
    throw "Failed to compile rathole-ui binary at $webUiBin!"
}
$webUiSize = (Get-Item $webUiBin).Length / 1MB
Write-Host "   -> rathole-ui compiled successfully! Size: $('{0:N2}' -f $webUiSize) MB" -ForegroundColor Green

# ---------------------------------------------------------
# 2. Prepare Rathole core binary
# ---------------------------------------------------------
Write-Host "`n[2/5] Preparing rathole core binary..." -ForegroundColor Yellow
$cacheArch = if ($Arch -in @("aarch64", "arm64", "rtd1296")) { "aarch64" } else { "x86_64" }
$archCacheDir = "$CACHE_DIR/$cacheArch"
$ratholeBin = "$archCacheDir/rathole"

if (!(Test-Path $ratholeBin)) {
    New-Item -ItemType Directory -Force $archCacheDir | Out-Null
    $assetName = if ($cacheArch -eq "x86_64") {
        "rathole-x86_64-unknown-linux-gnu.zip"
    } else {
        "rathole-aarch64-unknown-linux-musl.zip"
    }

    $downloadUrl = "https://github.com/rathole-org/rathole/releases/download/$RatholeVersion/$assetName"
    $zipPath = "$CACHE_DIR/$assetName"
    
    if (!(Test-Path $zipPath)) {
        Write-Host "   Downloading $assetName from GitHub ($RatholeVersion)..." -ForegroundColor Cyan
        Invoke-WebRequest -Uri $downloadUrl -OutFile $zipPath -UserAgent "PowerShell"
    }
    
    Write-Host "   Extracting $assetName..." -ForegroundColor Cyan
    Expand-Archive -Path $zipPath -DestinationPath $archCacheDir -Force
}

if (!(Test-Path $ratholeBin)) {
    throw "Rathole binary not found at $ratholeBin"
}
$ratholeSize = (Get-Item $ratholeBin).Length / 1MB
Write-Host "   -> rathole core binary ready! Size: $('{0:N2}' -f $ratholeSize) MB" -ForegroundColor Green

# ---------------------------------------------------------
# 3. Assemble package payload (package.tgz)
# ---------------------------------------------------------
Write-Host "`n[3/5] Packaging payload (package.tgz)..." -ForegroundColor Yellow
$pkgPayloadDir = "$BUILD_DIR/payload"
New-Item -ItemType Directory -Force "$pkgPayloadDir/bin" | Out-Null
New-Item -ItemType Directory -Force "$pkgPayloadDir/etc" | Out-Null
New-Item -ItemType Directory -Force "$pkgPayloadDir/var" | Out-Null
New-Item -ItemType Directory -Force "$pkgPayloadDir/ui" | Out-Null

Copy-Item $webUiBin "$pkgPayloadDir/bin/rathole-ui" -Force
[System.IO.File]::WriteAllBytes("$pkgPayloadDir/bin/rathole", [System.IO.File]::ReadAllBytes($ratholeBin))
Copy-Item "$SRC_DIR/spk/etc/config.toml.default" "$pkgPayloadDir/etc/config.toml.default" -Force
Copy-Item "$SRC_DIR/spk/etc/config.toml.default" "$pkgPayloadDir/var/config.toml" -Force
Copy-Item -Recurse "$SRC_DIR/spk/ui/*" "$pkgPayloadDir/ui/" -Force

# Normalize CGI and script line endings (strict LF '\n')
Get-ChildItem "$pkgPayloadDir/ui" -Filter "*.cgi" -File | ForEach-Object {
    $c = [System.IO.File]::ReadAllText($_.FullName, $utf8NoBom)
    $c = $c -replace "\r\n", "`n"
    [System.IO.File]::WriteAllText($_.FullName, $c, $utf8NoBom)
}

# Create package.tar and compress to package.tgz
$payloadTar = "$BUILD_DIR/package.tar"
$payloadTgz = "$BUILD_DIR/package.tgz"
tar -cf $payloadTar -C $pkgPayloadDir bin etc ui var
[SpkTarFixer]::MakeExecutable($payloadTar)
Compress-GzipFile $payloadTar $payloadTgz
Remove-Item $payloadTar -Force
Write-Host "   -> package.tgz created!" -ForegroundColor Green

# ---------------------------------------------------------
# 4. Assemble SPK Root Structure
# ---------------------------------------------------------
Write-Host "`n[4/5] Assembling SPK metadata and lifecycle scripts..." -ForegroundColor Yellow
$spkRootDir = "$BUILD_DIR/spk_root"
New-Item -ItemType Directory -Force $spkRootDir | Out-Null
New-Item -ItemType Directory -Force "$spkRootDir/scripts" | Out-Null

# Copy SPK root files
Copy-Item "$SRC_DIR/spk/INFO" "$spkRootDir/INFO" -Force
Copy-Item "$SRC_DIR/spk/PACKAGE_ICON.PNG" "$spkRootDir/PACKAGE_ICON.PNG" -Force
Copy-Item "$SRC_DIR/spk/PACKAGE_ICON_256.PNG" "$spkRootDir/PACKAGE_ICON_256.PNG" -Force

$spkEntries = [System.Collections.Generic.List[string]]::new()
$spkEntries.Add("INFO")
$spkEntries.Add("PACKAGE_ICON.PNG")
$spkEntries.Add("PACKAGE_ICON_256.PNG")
$spkEntries.Add("scripts")
$spkEntries.Add("package.tgz")

if (Test-Path "$SRC_DIR/spk/conf") {
    New-Item -ItemType Directory -Force "$spkRootDir/conf" | Out-Null
    Copy-Item -Recurse "$SRC_DIR/spk/conf/*" "$spkRootDir/conf/" -Force
    $spkEntries.Add("conf")
}

Copy-Item -Recurse "$SRC_DIR/spk/scripts/*" "$spkRootDir/scripts/" -Force
Copy-Item $payloadTgz "$spkRootDir/package.tgz" -Force

# Normalize INFO file (UTF-8 without BOM, strict LF '\n')
$infoFile = "$spkRootDir/INFO"
$infoContent = [System.IO.File]::ReadAllText($infoFile, $utf8NoBom)
$infoContent = $infoContent -replace "\r\n", "`n"
$infoContent = $infoContent -replace 'version="[^"]*"', "version=`"$Version`""
if ($Arch -in @("aarch64", "arm64", "rtd1296")) {
    $infoContent = $infoContent -replace 'arch="[^"]*"', 'arch="rtd1296 rtd1619b aarch64 armada37xx armada38x"'
} else {
    $infoContent = $infoContent -replace 'arch="[^"]*"', 'arch="x86_64 broadwell broadwellnk apollolake geminilake denverton v1000 r1000 purley braswell bromolow cedarview avoton grantley banff"'
}
[System.IO.File]::WriteAllText($infoFile, $infoContent, $utf8NoBom)

# Normalize all shell scripts (strict LF '\n', UTF-8 without BOM)
Get-ChildItem "$spkRootDir/scripts" -File | ForEach-Object {
    $c = [System.IO.File]::ReadAllText($_.FullName, $utf8NoBom)
    $c = $c -replace "\r\n", "`n"
    [System.IO.File]::WriteAllText($_.FullName, $c, $utf8NoBom)
}

# ---------------------------------------------------------
# 5. Pack final .spk file
# ---------------------------------------------------------
Write-Host "`n[5/5] Creating final SPK archive: $OUTPUT_SPK..." -ForegroundColor Yellow
$tempSpkTar = "$BUILD_DIR/rathole.tar"

tar -cf $tempSpkTar -C $spkRootDir $spkEntries
[SpkTarFixer]::MakeExecutable($tempSpkTar)

Move-Item $tempSpkTar $OUTPUT_SPK -Force

$finalSize = (Get-Item $OUTPUT_SPK).Length / 1MB
Write-Host "`n==========================================================" -ForegroundColor Green
Write-Host " BUILD SUCCESSFUL!" -ForegroundColor Green
Write-Host " Output SPK: $OUTPUT_SPK" -ForegroundColor Green
Write-Host " Package Size: $('{0:N2}' -f $finalSize) MB" -ForegroundColor Green
Write-Host "==========================================================" -ForegroundColor Green
