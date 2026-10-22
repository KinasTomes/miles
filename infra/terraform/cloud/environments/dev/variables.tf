variable "region" {
  type    = string
  default = "ap-southeast-1"
  validation {
    condition     = var.region == "ap-southeast-1"
    error_message = "W2 dev is pinned to Singapore; review sources/costs before changing Region."
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
    condition     = var.runtime_permissions_boundary_arn == null ? true : var.runtime_permissions_boundary_arn == "arn:aws:iam::${var.account_id}:policy/miles/bootstrap/MilesDevRuntimeBoundary"
    error_message = "Use the reviewed Miles dev runtime boundary in this account."
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
  default = ["ap-southeast-1a", "ap-southeast-1b"]
  validation {
    condition     = length(var.availability_zones) == 2 && length(distinct(var.availability_zones)) == 2 && alltrue([for z in var.availability_zones : can(regex("^ap-southeast-1[a-z]$", z))])
    error_message = "Supply two distinct Singapore AZs."
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
variable "nat_per_az" {
  type        = bool
  default     = false
  description = "Dev NAT mode uses one NAT; true gives one per AZ at higher cost."
}
variable "enable_alb" {
  type    = bool
  default = false
  validation {
    condition     = !var.enable_alb || (var.certificate_arn != null && length(var.client_cidrs) > 0)
    error_message = "Enabling ALB requires a real ACM certificate and client CIDR allowlist."
  }
}
variable "certificate_arn" {
  type    = string
  default = null
  validation {
    condition     = var.certificate_arn == null ? true : can(regex("^arn:aws:acm:ap-southeast-1:[0-9]{12}:certificate/.+", var.certificate_arn))
    error_message = "ACM certificate must be in Singapore."
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
