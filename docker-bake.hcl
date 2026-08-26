variable "VERSION" { default = "dev" }
variable "OCI_VERSION" { default = "dev" }
variable "COMMIT" { default = "unknown" }
variable "REVISION" { default = "unknown" }
variable "BUILD_DATE" { default = "" }
variable "OCI_OUTPUT_DIR" { default = "./dist" }

target "oci" {
  name = "oci-${platform.architecture}"
  matrix = {
    platform = [
      {
        os           = "linux"
        architecture = "amd64"
      },
      {
        os           = "linux"
        architecture = "arm64"
      },
    ]
  }

  context    = "."
  dockerfile = "Dockerfile"
  platforms  = ["${platform.os}/${platform.architecture}"]

  args = {
    VERSION    = VERSION
    COMMIT     = COMMIT
    BUILD_DATE = BUILD_DATE
  }

  labels = {
    "org.opencontainers.image.created"     = BUILD_DATE
    "org.opencontainers.image.description" = "Local web viewer for AI agent sessions"
    "org.opencontainers.image.licenses"    = "MIT"
    "org.opencontainers.image.revision"    = REVISION
    "org.opencontainers.image.source"      = "https://github.com/kenn-io/agentsview"
    "org.opencontainers.image.title"       = "AgentsView"
    "org.opencontainers.image.url"         = "https://github.com/kenn-io/agentsview"
    "org.opencontainers.image.version"     = OCI_VERSION
  }

  output = [{
    type             = "oci"
    dest             = "${OCI_OUTPUT_DIR}/${platform.architecture}.oci.tar"
    "oci-mediatypes" = true
  }]
}
