@{
    Severity = @('Error', 'Warning')
    ExcludeRules = @(
        # These are interactive operator scripts. Write-Host is the intended output.
        'PSAvoidUsingWriteHost'
    )
}
