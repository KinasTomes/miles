variable "region" {
  type    = string
  default = "ap-southeast-1"
  validation {
    condition     = can(regex("^(af|ap|ca|eu|il|me|mx|sa|us)-[a-z]+-[0-9]+$", var.region))
    error_message = "Use a commercial AWS Region and review service availability and costs."
  }
}
variable "account_id" {
  type = string
  validation {
    condition     = can(regex("^[0-9]{12}$", var.account_id))
    error_message = "Account ID must contain 12 digits."
  }
}
variable "deploy_role_arn" {
  type        = string
  default     = null
  description = "Existing scoped deployment role; root is allowed for read-only plan only, never apply."
  validation {
    condition     = var.deploy_role_arn == null ? true : can(regex("^arn:aws:iam::[0-9]{12}:role/.+", var.deploy_role_arn))
    error_message = "Supply an IAM role ARN, not a user/root ARN."
  }
}
variable "runtime_permissions_boundary_arn" {
  type        = string
  default     = null
  description = "Pre-created runtime boundary; the deployment role requires it when creating EC2 roles."
  validation {
    condition     = var.runtime_permissions_boundary_arn == null ? true : can(regex("^arn:aws:iam::${var.account_id}:policy/[A-Za-z0-9_+=,.@/-]+$", var.runtime_permissions_boundary_arn))
    error_message = "Use an explicitly reviewed permissions boundary in the target account."
  }
}
variable "vpc_cidr" {
  type    = string
  default = "10.0.0.0/16"
  validation {
    condition     = can(cidrnetmask(var.vpc_cidr)) && can(cidrsubnet(var.vpc_cidr, 8, 101)) && try(tonumber(split("/", var.vpc_cidr)[1]) >= 16 && tonumber(split("/", var.vpc_cidr)[1]) <= 19, false)
    error_message = "Use IPv4 /16 through /19: /+8 subnets must be /27 or larger for ALB. Confirm no VPN overlap before apply."
  }
}
variable "availability_zones" {
  type    = list(string)
  default = null
  validation {
    condition     = var.availability_zones == null ? true : length(var.availability_zones) == 2 && length(distinct(var.availability_zones)) == 2 && alltrue([for z in var.availability_zones : can(regex("^${var.region}[a-z]$", z))])
    error_message = "Supply two distinct AZs in the configured Region."
  }
}
variable "egress_mode" {
  type    = string
  default = "isolated"
  validation {
    condition     = contains(["isolated", "nat", "interface"], var.egress_mode)
    error_message = "Choose isolated (foundation only), nat, or interface."
  }
}
variable "paused" {
  type        = bool
  default     = true
  description = "Remove EC2/root disk and paid session networking; retain data volume, S3/DDB, network, IAM, keys and state. Retained resources still incur storage/key costs."
}
variable "enable_compute" {
  type        = bool
  default     = false
  description = "Include the single-node Warm dev EC2 and retained data volume. Keep true across pause/resume."
  validation {
    condition     = !var.enable_compute || var.paused || (var.ami_id != null && var.egress_mode != "isolated")
    error_message = "Active dev compute needs a reviewed AMI and NAT/interface API egress."
  }
}
variable "ami_id" {
  type    = string
  default = null
  validation {
    condition     = var.ami_id == null ? true : can(regex("^ami-[0-9a-f]{17}$", var.ami_id))
    error_message = "Use a pinned regional AL2023 AMI matching the configured architecture."
  }
}
variable "nat_per_az" {
  type        = bool
  default     = false
  description = "Dev NAT mode uses one NAT; true gives one per AZ at higher cost."
}
variable "enable_asg" {
  type        = bool
  default     = false
  description = "Cloud two-AZ fleet instead of retained single-node compute; prepare only until runtime integration and costs are reviewed."
  validation {
    condition = !var.enable_asg || (
      !var.enable_compute && var.enable_alb && var.runtime_permissions_boundary_arn != null &&
      var.ami_id != null && var.fleet_instance_type != null &&
      can(regex("^[a-f0-9]{64}$", var.release_sha256)) && var.release_version_id != null &&
      (var.egress_mode == "interface" || (var.egress_mode == "nat" && var.nat_per_az))
    )
    error_message = "Fleet needs ALB/TLS, reviewed boundary/AMI/type/versioned release and two-AZ API egress; it cannot share single-node dev compute."
  }
}
variable "fleet_instance_type" {
  type        = string
  default     = null
  description = "Explicit cost/capacity-reviewed EC2 type; no workload sizing is inferred."
  validation {
    condition     = var.fleet_instance_type == null ? true : can(regex("^[a-z0-9-]+\\.[a-z0-9]+$", var.fleet_instance_type))
    error_message = "Supply a reviewed EC2 instance type."
  }
}
variable "release_architecture" {
  type    = string
  default = "arm64"
  validation {
    condition     = contains(["arm64", "amd64"], var.release_architecture)
    error_message = "Release must be Linux arm64 or amd64 and match the AMI/instance."
  }
}
variable "release_sha256" {
  type    = string
  default = null
  validation {
    condition     = var.release_sha256 == null ? true : can(regex("^[a-f0-9]{64}$", var.release_sha256))
    error_message = "Supply the exact release archive SHA-256."
  }
}
variable "release_version_id" {
  type    = string
  default = null
  validation {
    condition     = var.release_version_id == null ? true : length(var.release_version_id) > 0 && length(var.release_version_id) <= 1024 && var.release_version_id != "null" && trimspace(var.release_version_id) == var.release_version_id
    error_message = "Use a non-null immutable S3 version ID, not an unversioned object."
  }
}
variable "enable_alb" {
  type    = bool
  default = false
  validation {
    condition     = !var.enable_alb || (var.certificate_arn != null && length(var.client_cidrs) > 0)
    error_message = "Enabling ALB requires a real ACM certificate and client CIDR allowlist."
  }
}
variable "alb_deregistration_delay_seconds" {
  type        = number
  default     = 1800
  nullable    = false
  description = "ALB drain wait in whole seconds (0..1800). Use 300 for bounded dev/test requests; default preserves the 30-minute drain."
}
variable "certificate_arn" {
  type    = string
  default = null
  validation {
    condition     = var.certificate_arn == null ? true : can(regex("^arn:aws:acm:${var.region}:${var.account_id}:certificate/[A-Za-z0-9-]+$", var.certificate_arn))
    error_message = "ACM certificate must belong to the configured account and Region."
  }
}
variable "client_cidrs" {
  type    = set(string)
  default = []
  validation {
    condition     = alltrue([for c in var.client_cidrs : can(cidrnetmask(c))])
    error_message = "Client allowlist must contain IPv4 CIDRs."
  }
}
variable "admin_ssh_cidrs" {
  type    = set(string)
  default = []
  validation {
    condition     = alltrue([for c in var.admin_ssh_cidrs : can(cidrnetmask(c)) && !contains(["0.0.0.0/0"], c)])
    error_message = "Bastion SSH must use restricted IPv4 CIDRs."
  }
}
variable "owner_ssh_public_key" {
  type        = string
  default     = null
  description = "Public ed25519 key for restricted owner forwarding through an approved private SSH/SSM route."
}
variable "edge_ca_pem" {
  type        = string
  default     = null
  description = "Public PEM CA certificate enables the separate Roles Anywhere Edge runtime role."
}
variable "runtime_secret_name" {
  type        = string
  default     = null
  description = "Fresh installation secret name; null preserves the original installation name."
}

variable "installation_name" {
  type    = string
  default = "miles-dev"
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,22}[a-z0-9]$", var.installation_name)) && length("${var.installation_name}-tfstate-${var.account_id}-${var.region}") <= 63
    error_message = "Use a unique 3..24 character lowercase name fitting the regional S3 bucket length limit."
  }
}
variable "environment" {
  type    = string
  default = "dev"
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,19}$", var.environment))
    error_message = "Use a 1..20 character environment tag."
  }
}
variable "owner" {
  type    = string
  default = "operator"
  validation {
    condition     = length(trimspace(var.owner)) > 0 && length(var.owner) <= 128
    error_message = "Supply an owner tag of 1..128 characters."
  }
}
variable "runtime_iam_path" {
  type    = string
  default = "/miles/dev/"
  validation {
    condition     = can(regex("^/([A-Za-z0-9_+=,.@-]+/)*$", var.runtime_iam_path)) && length(var.runtime_iam_path) <= 128
    error_message = "Supply an IAM path beginning and ending with a slash."
  }
}
variable "edge_certificate_common_name" {
  type    = string
  default = "miles-edge"
  validation {
    condition     = can(regex("^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$", var.edge_certificate_common_name))
    error_message = "Supply the exact reviewed Edge certificate common name."
  }
}
variable "instance_type" {
  type    = string
  default = "t4g.small"
  validation {
    condition     = can(regex("^[a-z0-9-]+\\.[a-z0-9]+$", var.instance_type))
    error_message = "Supply the reviewed single-node EC2 instance type."
  }
}

variable "enable_edge" {
  type    = bool
  default = false
  validation {
    condition     = !var.enable_edge || (var.edge_ca_pem != null && var.runtime_permissions_boundary_arn != null && var.ami_id != null && length(var.client_cidrs) > 0 && can(regex("^[a-f0-9]{64}$", var.edge_helper_sha256)) && var.edge_helper_version_id != null && can(regex("^[a-f0-9]{64}$", var.release_sha256)) && var.release_version_id != null)
    error_message = "Edge needs its public CA, reviewed boundary/AMI/client allowlist and pinned release/helper versions."
  }
}
variable "edge_instance_type" {
  type    = string
  default = "t4g.small"
  validation {
    condition     = can(regex("^[a-z0-9-]+\\.[a-z0-9]+$", var.edge_instance_type))
    error_message = "Supply the reviewed Edge instance type."
  }
}
variable "edge_helper_sha256" {
  type    = string
  default = null
}
variable "edge_helper_version_id" {
  type    = string
  default = null
}
