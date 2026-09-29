"""Build source-derived release archives; never uploads or deploys.
Run in a clean public checkout:
  python deploy/package-release.py --version 0.1.0 --commit <full-public-commit> --out <new-directory>
Dependencies: Go, Node.js/npm, Python 3.10+. Use npm ci in web/ first.
The caller must verify the checkout matches --commit before building.
"""
import argparse, hashlib, io, json, os, pathlib, re, shutil, subprocess, tarfile, zipfile

def main():
    p = argparse.ArgumentParser()
    p.add_argument("--version", required=True)
    p.add_argument("--commit", required=True)
    p.add_argument("--out", required=True)
    a = p.parse_args()
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", a.version):
        p.error("version must be X.Y.Z (without v)")
    if not re.fullmatch(r"[0-9a-f]{40}", a.commit):
        p.error("commit must be the full public source commit")
    root = pathlib.Path(__file__).resolve().parents[1]
    out = pathlib.Path(a.out).resolve()
    out.mkdir(parents=True, exist_ok=False)
    work = out / "build"
    work.mkdir()
    env = os.environ.copy()
    env.update(CGO_ENABLED="0", GOMAXPROCS="2", TH_WEB_VERSION=a.version)
    npm = "npm.cmd" if os.name == "nt" else "npm"
    subprocess.run([npm, "run", "build"], cwd=root/"web", env=env, check=True)
    gov = subprocess.check_output(["go", "version"], text=True).strip()
    provenance = json.dumps({"version": a.version, "source": "https://github.com/czgreat/termhub",
                             "commit": a.commit, "go": gov}, indent=2).encode()
    shared = {name: (root/name).read_bytes() for name in ("LICENSE", "THIRD_PARTY_LICENSES")}
    shared["BUILDINFO.json"] = provenance
    shared["README.md"] = (root/"docs/发布包安装.md").read_bytes()
    for name in ("部署指南.md", "安全说明.md", "发布包安装.md"):
        shared["docs/"+name] = (root/"docs"/name).read_bytes()
    paths = []
    for platform, arch in [("windows", "amd64"), ("linux", "amd64"), ("linux", "arm64")]:
        agent = platform == "windows"
        binary = "termhub-agent.exe" if agent else "termhub"
        target = work/(platform+"-"+arch+"-"+binary)
        buildenv = dict(env, GOOS=platform, GOARCH=arch)
        subprocess.run(["go", "build", "-p", "1", "-buildvcs=false", "-trimpath", "-ldflags",
                        "-s -w -X main.version="+a.version, "-o", str(target),
                        "./cmd/termhub-agent" if agent else "./cmd/termhub"], cwd=root, env=buildenv, check=True)
        files = dict(shared)
        files[binary] = target.read_bytes()
        stem = "termhub_"+a.version+"_"+platform+"_"+arch
        if agent:
            for name in ("install-agent.ps1", "upgrade-agent.ps1", "uninstall-agent.ps1"):
                files[name] = (root/"deploy"/name).read_bytes()
            path = out/(stem+".zip")
            with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as z:
                for name, data in files.items(): z.writestr(name, data)
        else:
            for name in ("docker-compose.yml", ".env.example", "hub.env.example"):
                files[name] = (root/"deploy"/name).read_bytes()
            files["Dockerfile"] = (root/"deploy/Dockerfile.prebuilt").read_bytes()
            files["etc/passwd"] = b"nonroot:x:65532:65532:nonroot:/:/sbin/nologin\n"
            files["etc/group"] = b"nonroot:x:65532:\n"
            files["data/.keep"] = b""
            path = out/(stem+".tar.gz")
            with tarfile.open(path, "w:gz") as t:
                for name, data in files.items():
                    info = tarfile.TarInfo(name)
                    info.size = len(data)
                    info.mode = 0o755 if name == binary else 0o644
                    info.mtime = 0
                    t.addfile(info, io.BytesIO(data))
        paths.append(path)
    (out/"SHA256SUMS.txt").write_text("".join(hashlib.sha256(p.read_bytes()).hexdigest()+"  "+p.name+"\n" for p in paths), encoding="utf-8")
    print("Release archives created:", *(p.name for p in paths))

if __name__ == "__main__":
    main()
