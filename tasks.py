import os
import semver
import shutil
import sys
import yaml
import tempfile
try:
    from urllib.request import urlopen
except ImportError:
    from urllib2 import urlopen

from invoke import run, task
from invoke.exceptions import Exit

all_binaries = set(["controller-pool",
                    "controller-acnodal",
                    "speaker-acnodal",
                    "speaker-local"])
all_architectures = set(["amd64",
                         "arm",
                         "arm64",
                         "ppc64le",
                         "s390x"])

def _check_architectures(architectures):
    out = set()
    for arch in architectures:
        if arch == "all":
            out |= all_architectures
        elif arch not in all_architectures:
            print("unknown architecture {}".format(arch))
            print("Supported architectures: {}".format(", ".join(sorted(all_architectures))))
            sys.exit(1)
        else:
            out.add(arch)
    if not out:
        out.add("amd64")
    return list(sorted(out))

def _check_binaries(binaries):
    out = set()
    for binary in binaries:
        if binary == "all":
            out |= all_binaries
        elif binary not in all_binaries:
            print("Unknown binary {}".format(binary))
            print("Known binaries: {}".format(", ".join(sorted(all_binaries))))
            sys.exit(1)
        else:
            out.add(binary)
    if not out:
        out |= all_binaries
    return list(sorted(out))

def _make_build_dirs():
    for arch in all_architectures:
        for binary in all_binaries:
            dir = os.path.join("build", arch, binary)
            if not os.path.exists(dir):
                os.makedirs(dir, mode=0o750)

@task(iterable=["binaries", "architectures"],
      help={
          "binaries": "binaries to build. One or more of {}, or 'all'".format(", ".join(sorted(all_binaries))),
          "architectures": "architectures to build. One or more of {}, or 'all'".format(", ".join(sorted(all_architectures))),
          "tag": "docker image tag prefix to use. Actual tag will be <tag>-<arch>. Default 'dev'.",
          "docker-user": "docker user under which to tag the images. Default 'metallb'.",
      })
def build(ctx, binaries, architectures, tag="dev", docker_user="metallb"):
    """Build MetalLB docker images."""
    binaries = _check_binaries(binaries)
    architectures = _check_architectures(architectures)
    _make_build_dirs()

    commit = run("git describe --dirty --always", hide=True).stdout.strip()
    branch = run("git rev-parse --abbrev-ref HEAD", hide=True).stdout.strip()

    for arch in architectures:
        env = {
            "CGO_ENABLED": "0",
            "GOOS": "linux",
            "GOARCH": arch,
            "GOARM": "6",
            "GO111MODULE": "on",
        }
        for bin in binaries:
            run("go build -v -o build/{arch}/{bin}/{bin} -ldflags "
                "'-X go.universe.tf/metallb/internal/version.gitCommit={commit} "
                "-X go.universe.tf/metallb/internal/version.gitBranch={branch}' "
                "./cmd/{bin}/".format(
                    arch=arch,
                    bin=bin,
                    commit=commit,
                    branch=branch),
                env=env,
                echo=True)
            run("docker build "
                "-t {user}/{bin}:{tag}-{arch} "
                "-f build/package/Dockerfile.{bin} build/{arch}/{bin}".format(
                    user=docker_user,
                    bin=bin,
                    tag=tag,
                    arch=arch),
                echo=True)

@task(iterable=["binaries", "architectures"],
      help={
          "binaries": "binaries to build. One or more of {}, or 'all'".format(", ".join(sorted(all_binaries))),
          "architectures": "architectures to build. One or more of {}, or 'all'".format(", ".join(sorted(all_architectures))),
          "tag": "docker image tag prefix to use. Actual tag will be <tag>-<arch>. Default 'dev'.",
          "docker-user": "docker user under which to tag the images. Default 'metallb'.",
      })
def push(ctx, binaries, architectures, tag="dev", docker_user="metallb"):
    """Build and push docker images to registry."""
    binaries = _check_binaries(binaries)
    architectures = _check_architectures(architectures)

    for arch in architectures:
        for bin in binaries:
            build(ctx, binaries=[bin], architectures=[arch], tag=tag, docker_user=docker_user)
            run("docker push {user}/{bin}:{tag}-{arch}".format(
                user=docker_user,
                bin=bin,
                arch=arch,
                tag=tag),
                echo=True)

@task(iterable=["binaries"],
      help={
          "binaries": "binaries to build. One or more of {}, or 'all'".format(", ".join(sorted(all_binaries))),
          "tag": "docker image tag prefix to use. Actual tag will be <tag>-<arch>. Default 'dev'.",
          "docker-user": "docker user under which to tag the images. Default 'metallb'.",
      })
def push_multiarch(ctx, binaries, tag="dev", docker_user="metallb"):
    """Build and push multi-architecture docker images to registry."""
    binaries = _check_binaries(binaries)
    architectures = _check_architectures(["all"])
    push(ctx, binaries=binaries, architectures=architectures, tag=tag, docker_user=docker_user)

    platforms = ",".join("linux/{}".format(arch) for arch in architectures)
    for bin in binaries:
        run("manifest-tool push from-args "
            "--platforms {platforms} "
            "--template {user}/{bin}:{tag}-ARCH "
            "--target {user}/{bin}:{tag}".format(
                platforms=platforms,
                user=docker_user,
                bin=bin,
                tag=tag),
            echo=True)

@task(help={
    "architecture": "CPU architecture of the local machine. Default 'amd64'.",
    "name": "name of the kind cluster to use.",
})
def release(ctx, version, skip_release_notes=False):
    """Tag a new release."""
    status = run("git status --porcelain", hide=True).stdout.strip()
    if status != "":
        raise Exit(message="git checkout not clean, cannot release")

    version = semver.parse_version_info(version)
    is_patch_release = version.patch != 0

    # Move HEAD to the correct release branch - either a new one, or
    # an existing one.
    if is_patch_release:
        run("git checkout v{}.{}".format(version.major, version.minor), echo=True)
    else:
        run("git checkout -b v{}.{}".format(version.major, version.minor), echo=True)

    # Update the manifests with the new version
    run("perl -pi -e 's,image: metallb/speaker:.*,image: metallb/speaker:v{},g' deployments/metallb.yaml".format(version), echo=True)
    run("perl -pi -e 's,image: metallb/controller:.*,image: metallb/controller:v{},g' deployments/metallb.yaml".format(version), echo=True)

    # Update the version embedded in the binary
    run("perl -pi -e 's/version\s+=.*/version = \"{}\"/g' internal/version/version.go".format(version), echo=True)
    run("gofmt -w internal/version/version.go", echo=True)

    run("git commit -a -m 'Automated update for release v{}'".format(version), echo=True)
    run("git checkout main", echo=True)
