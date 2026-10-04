import { useId } from 'react'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from './ui/select'

type Option = { value: string; label: string }
type AppSelectProps = {
  value: string
  onValueChange: (value: string) => void
  options: Option[]
  ariaLabel?: string
  labelledBy?: string
  placeholder?: string
  className?: string
}

const emptyValue = '__cline_empty_option__'

export function AppSelect({ value, onValueChange, options, ariaLabel, labelledBy, placeholder, className }: AppSelectProps) {
  const hasEmptyOption = options.some(option => option.value === '')
  return <Select value={value === '' ? hasEmptyOption ? emptyValue : undefined : value} onValueChange={next => onValueChange(next === emptyValue ? '' : next)}>
    <SelectTrigger aria-label={ariaLabel} aria-labelledby={labelledBy} className={className || 'w-full min-w-0'}>
      <SelectValue placeholder={placeholder}/>
    </SelectTrigger>
    <SelectContent position="popper" sideOffset={6} align="start">
      {options.map(option => <SelectItem key={option.value} value={option.value === '' ? emptyValue : option.value}>{option.label}</SelectItem>)}
    </SelectContent>
  </Select>
}

export function SelectField({ label, ...props }: AppSelectProps & { label: string }) {
  const id = useId()
  return <div className="flex min-w-0 flex-col gap-2 text-sm font-medium">
    <span id={id}>{label}</span>
    <AppSelect {...props} labelledBy={id}/>
  </div>
}
