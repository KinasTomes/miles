variable "region" {
  type    = string
  default = "ap-southeast-1"
  validation {
    condition     = can(regex("^(af|ap|ca|eu|il|me|mx|sa|us)-[a-z]+-[0-9]+$", var.region))
    error_message = "Use a commercial AWS Region and review service availability and costs."
  }
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
  default = "Hung"
  validation {
    condition     = length(trimspace(var.owner)) > 0 && length(var.owner) <= 128
    error_message = "Supply an owner tag of 1..128 characters."
  }
}
